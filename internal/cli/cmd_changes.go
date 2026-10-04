package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/state"
)

func init() {
	register("changes", command{
		usage:   "changes [--folder FOLDER_ID] [--peek] [--reset]",
		summary: "List what changed since the last call (the first call only starts tracking).",
		locked:  true,
		run:     runChanges,
	})
	register("watch", command{
		usage:   "watch [--folder FOLDER_ID] [--interval 60s]",
		summary: "Poll for changes and print one JSON line per change.",
		run:     runWatch,
	})
}

// Change is one changed file.
type Change struct {
	FileID       string   `json:"fileId"`
	Name         string   `json:"name,omitempty"`
	MimeType     string   `json:"mimeType,omitempty"`
	ModifiedTime string   `json:"modifiedTime,omitempty"`
	Parents      []string `json:"parents,omitempty"`
	Trashed      bool     `json:"trashed,omitempty"`
	Removed      bool     `json:"removed,omitempty"`
	Time         string   `json:"time"`
}

type feed struct {
	Token string `json:"token"`
	// Known holds ids seen inside the folder, so later permanent removals can be reported.
	Known map[string]bool `json:"known,omitempty"`
}

type feeds map[string]*feed

func feedKey(folder string) string {
	if folder == "" {
		return "all"
	}
	return "folder:" + folder
}

func loadFeeds(d state.Dir) (feeds, error) {
	f := feeds{}
	err := state.ReadJSON(d.ChangesFile(), &f)
	if errors.Is(err, os.ErrNotExist) {
		return feeds{}, nil
	}
	return f, err
}

// poll is one read of a change feed. Commit stores the advanced position; call it only after
// the changes have been delivered, so a failed write never loses them. The caller must hold
// the state lock from poll until Commit.
type poll struct {
	Changes     []Change
	Initialized bool
	Commit      func() error
}

// pollChanges returns changes since the stored position. With no stored position it starts
// tracking (recording the files already in the folder) and returns Initialized=true.
func pollChanges(ctx context.Context, a *App, folder string, reset bool) (poll, error) {
	c, err := a.Clients(ctx)
	if err != nil {
		return poll{}, err
	}
	all, err := loadFeeds(a.Dir)
	if err != nil {
		return poll{}, err
	}
	key := feedKey(folder)
	fd := all[key]
	if fd == nil || fd.Token == "" || reset {
		if folder != "" {
			f, err := c.Drive.Files.Get(folder).Fields("id,mimeType").Context(ctx).Do()
			if err != nil {
				return poll{}, err
			}
			if f.MimeType != folderMime {
				return poll{}, apperr.Newf(apperr.Usage, "%s is not a folder", folder)
			}
		}
		// Take the position first: anything that changes while the folder is listed is
		// reported by the next call.
		st, err := c.Drive.Changes.GetStartPageToken().Context(ctx).Do()
		if err != nil {
			return poll{}, err
		}
		known := map[string]bool{}
		if folder != "" {
			if known, err = folderTree(ctx, c.Drive, folder); err != nil {
				return poll{}, err
			}
		}
		all[key] = &feed{Token: st.StartPageToken, Known: known}
		return poll{Changes: []Change{}, Initialized: true,
			Commit: func() error { return state.WriteJSON(a.Dir.ChangesFile(), all) }}, nil
	}
	if fd.Known == nil {
		fd.Known = map[string]bool{}
	}

	in := newAncestry(ctx, c.Drive, folder)
	byID := map[string]int{}
	out := []Change{}
	token := fd.Token
	for {
		resp, err := c.Drive.Changes.List(token).IncludeRemoved(true).PageSize(1000).Spaces("drive").
			Fields("nextPageToken,newStartPageToken,changes(fileId,removed,time,file(id,name,mimeType,modifiedTime,parents,trashed))").
			Context(ctx).Do()
		if err != nil {
			return poll{}, err
		}
		for _, ch := range resp.Changes {
			x := Change{FileID: ch.FileId, Removed: ch.Removed, Time: ch.Time}
			if ch.File != nil {
				x.Name, x.MimeType, x.ModifiedTime = ch.File.Name, ch.File.MimeType, ch.File.ModifiedTime
				x.Parents, x.Trashed = ch.File.Parents, ch.File.Trashed
			}
			if folder != "" {
				keep := false
				switch {
				case x.Removed || ch.File == nil:
					keep = fd.Known[x.FileID]
					delete(fd.Known, x.FileID)
				default:
					ok, err := in.contains(x.Parents)
					if err != nil {
						return poll{}, err
					}
					keep = ok
					if ok {
						fd.Known[x.FileID] = true
					} else {
						keep = fd.Known[x.FileID] // reported once when it moves out of the folder
						delete(fd.Known, x.FileID)
					}
				}
				if !keep {
					continue
				}
			}
			if i, seen := byID[x.FileID]; seen {
				out[i] = x
			} else {
				byID[x.FileID] = len(out)
				out = append(out, x)
			}
		}
		if resp.NewStartPageToken != "" {
			token = resp.NewStartPageToken
			break
		}
		token = resp.NextPageToken
	}
	return poll{Changes: out, Commit: func() error {
		fd.Token = token
		return state.WriteJSON(a.Dir.ChangesFile(), all)
	}}, nil
}

// folderTree returns the ids of everything inside folder, at any depth.
func folderTree(ctx context.Context, d *drive.Service, folder string) (map[string]bool, error) {
	known := map[string]bool{}
	queue := []string{folder}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		err := d.Files.List().Q(quoteQ(parent)+" in parents").PageSize(1000).
			Fields("nextPageToken,files(id,mimeType)").Pages(ctx, func(l *drive.FileList) error {
			for _, f := range l.Files {
				if known[f.Id] {
					continue
				}
				known[f.Id] = true
				if f.MimeType == folderMime {
					queue = append(queue, f.Id)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return known, nil
}

// ancestry answers "is this file inside folder (at any depth)?" with memoised parent lookups.
type ancestry struct {
	ctx    context.Context
	drive  *drive.Service
	folder string
	memo   map[string]bool
}

func newAncestry(ctx context.Context, d *drive.Service, folder string) *ancestry {
	return &ancestry{ctx: ctx, drive: d, folder: folder, memo: map[string]bool{folder: true}}
}

func (a *ancestry) contains(parents []string) (bool, error) {
	for _, p := range parents {
		ok, err := a.under(p, 0)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func (a *ancestry) under(id string, depth int) (bool, error) {
	if v, ok := a.memo[id]; ok {
		return v, nil
	}
	if depth > 64 {
		return false, nil
	}
	f, err := a.drive.Files.Get(id).Fields("id,parents").Context(a.ctx).Do()
	if err != nil {
		if apperrCode(err) == apperr.NotFound || apperrCode(err) == apperr.PermissionDenied {
			a.memo[id] = false // e.g. My Drive root's parent, or a folder we cannot see
			return false, nil
		}
		return false, err
	}
	res := false
	for _, p := range f.Parents {
		ok, err := a.under(p, depth+1)
		if err != nil {
			return false, err
		}
		if ok {
			res = true
			break
		}
	}
	a.memo[id] = res
	return res, nil
}

func runChanges(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "changes [--folder FOLDER_ID] [--peek] [--reset]"
	fs := newFlags("changes")
	folder := fs.String("folder", "", "only report files inside this folder (any depth)")
	peek := fs.Bool("peek", false, "do not advance the stored position")
	reset := fs.Bool("reset", false, "forget the stored position and start tracking from now")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, usage); err != nil {
		return nil, err
	}
	p, err := pollChanges(ctx, a, *folder, *reset)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"changes": p.Changes}
	if p.Initialized {
		if *peek {
			return nil, apperr.New(apperr.Usage, "nothing to peek at: this feed is not tracked yet").
				WithHint("Run `drivekey changes` once without --peek to start tracking.")
		}
		out["initialized"] = true
		out["note"] = "Tracking started. Changes from now on are reported by the next call."
	}
	// Print first and only then store the new position, so changes are never lost to a failed write.
	if err := writeJSON(a.Stdout, out); err != nil {
		return nil, err
	}
	if !*peek {
		if err := p.Commit(); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// watchOnce polls one feed under the state lock and commits only after every change was written.
func watchOnce(ctx context.Context, a *App, folder string) error {
	l, err := a.Dir.Lock(ctx, lockWait)
	if err != nil {
		return err
	}
	defer l.Unlock()
	p, err := pollChanges(ctx, a, folder, false)
	if err != nil {
		return err
	}
	for _, ch := range p.Changes {
		if err := writeCompactJSON(a.Stdout, ch); err != nil {
			return &deliveryError{err}
		}
	}
	return p.Commit()
}

// deliveryError means stdout is gone; watch stops instead of polling into the void.
type deliveryError struct{ err error }

func (e *deliveryError) Error() string { return "write changes: " + e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

func runWatch(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "watch [--folder FOLDER_ID] [--interval 60s]"
	fs := newFlags("watch")
	folder := fs.String("folder", "", "only report files inside this folder (any depth)")
	interval := fs.Duration("interval", time.Minute, "time between polls")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, usage); err != nil {
		return nil, err
	}
	if *interval < 5*time.Second {
		return nil, apperr.New(apperr.Usage, "--interval must be at least 5s")
	}
	for {
		if err := watchOnce(ctx, a, *folder); err != nil {
			if ctx.Err() != nil {
				return nil, nil
			}
			var de *deliveryError
			if errors.As(err, &de) {
				return nil, err
			}
			e := apperr.As(mapErr(err))
			switch e.Code {
			case apperr.NotLoggedIn, apperr.NotSetUp, apperr.Usage, apperr.NotFound, apperr.GcloudMissing:
				return nil, e
			}
			_ = writeCompactJSON(a.Stderr, map[string]any{"error": e})
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(*interval):
		}
	}
}
