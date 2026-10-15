package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/jerryfane/drivekey/internal/apperr"
	"github.com/jerryfane/drivekey/internal/gapi"
	"github.com/jerryfane/drivekey/internal/state"
)

func init() {
	register("changes", command{
		usage:   "changes [--folder FOLDER_ID | --drive SHARED_DRIVE_ID] [--peek] [--reset]",
		summary: "List what changed since the last call (the first call only starts tracking).",
		run:     runChanges,
	})
	register("watch", command{
		usage:   "watch [--folder FOLDER_ID | --drive SHARED_DRIVE_ID] [--interval 60s]",
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

// feed is one stored change-feed position.
type feed struct {
	Token string `json:"token"`
	// Gen identifies one tracking session; it changes on every start or reset, so a poll that
	// began before a reset can never overwrite the reset's position or membership.
	Gen string `json:"gen"`
	// Account and Session identify the login the feed was recorded under. A feed from any
	// other login, even of the same account, is ignored and never overwritten by its polls.
	// For a relay profile they are its relay/role/service identity and its profile id.
	Account string `json:"account"`
	Session string `json:"session"`
	// DriveID is set when the feed reads a shared drive's own change log.
	DriveID string `json:"driveId,omitempty"`
	// Known holds ids seen inside the folder, so later permanent removals can be reported.
	Known map[string]bool `json:"known,omitempty"`
}

type feeds map[string]*feed

// feedTarget is what a feed follows: a folder (at any depth), a whole shared drive, or,
// with both empty, everything the account's own change log reports.
type feedTarget struct{ Folder, Drive string }

func (t feedTarget) key() string {
	switch {
	case t.Folder != "":
		return "folder:" + t.Folder
	case t.Drive != "":
		return "drive:" + t.Drive
	}
	return "all"
}

func newFeedTarget(folder, sharedDrive string) (feedTarget, error) {
	if folder != "" && sharedDrive != "" {
		return feedTarget{}, apperr.New(apperr.Usage, "use --folder or --drive, not both").
			WithHint("A folder inside a shared drive is tracked through that drive's change log automatically.")
	}
	return feedTarget{Folder: folder, Drive: sharedDrive}, nil
}

// feedsFile is where the feeds of the clients' login are stored: relay profiles keep their
// own file, so no two accounts or relay routes ever share a position.
func feedsFile(d state.Dir, c *gapi.Clients) string {
	if c.Profile != "" {
		return d.ProfileChangesFile(c.Profile)
	}
	return d.ChangesFile()
}

func loadFeeds(path string) (feeds, error) {
	f := feeds{}
	err := state.ReadJSON(path, &f)
	if errors.Is(err, os.ErrNotExist) {
		return feeds{}, nil
	}
	return f, err
}

// poll is one read of a change feed. Commit stores the advanced position; call it only after
// the changes have been delivered, so a failed write never loses them.
//
// Polling and delivering hold no lock, so a slow reader never blocks other commands.
// Delivery is therefore at-least-once: two readers of the same feed running at the same
// time may both report a change. Commit takes the state lock and applies only if the feed
// is still the one the poll read, for the same account (so nothing survives a logout).
type poll struct {
	Changes     []Change
	Initialized bool
	Commit      func(ctx context.Context) error
}

// commitMode says when commitFeed may write.
type commitMode int

const (
	advance commitMode = iota // only if the stored feed still has the read generation and token
	start                     // only if no feed exists yet
	restart                   // always: an explicit --reset replaces whatever is stored
)

// commitFeed stores next under key if mode allows it and the login the clients act for is
// still current: the same direct login, or the same relay profile definition. read is the
// feed the poll started from (nil if none).
func commitFeed(ctx context.Context, a *App, c *gapi.Clients, key string, read, next *feed, mode commitMode) error {
	l, err := a.Dir.Lock(ctx, lockWait)
	if err != nil {
		return err
	}
	defer l.Unlock()
	account, session := "", ""
	if c.Profile != "" {
		profiles, err := a.Dir.LoadProfiles()
		if err != nil {
			return err
		}
		p, ok := profiles[c.Profile]
		if !ok || p.ID != next.Session {
			// Unlike a logout, this cannot heal: the clients still use the old definition.
			return apperr.Newf(apperr.ProfileNotFound, "relay profile %q was removed or redefined while this command ran; its position was not stored", c.Profile).
				WithHint("Run the command again.")
		}
		account, session = p.Identity(), p.ID
	} else {
		cfg, err := a.Dir.LoadConfig()
		if err != nil {
			return err
		}
		account, session = cfg.Account, cfg.Session
	}
	if account == "" || account != next.Account || session != next.Session {
		return nil // logged out or in again, or the profile was removed or redefined, since the poll began
	}
	path := feedsFile(a.Dir, c)
	all, err := loadFeeds(path)
	if err != nil {
		return err
	}
	cur := all[key]
	if cur != nil && (cur.Account != account || cur.Session != session) {
		cur = nil // left over from another login
	}
	switch mode {
	case start:
		if cur != nil {
			return nil // another reader started this feed first
		}
	case advance:
		if cur == nil || read == nil || cur.Gen != read.Gen || cur.Token != read.Token {
			return nil // advanced, reset or removed by someone else
		}
	}
	all[key] = next
	if c.Profile != "" {
		if err := a.Dir.EnsureProfileDir(c.Profile); err != nil {
			return err
		}
	}
	return state.WriteJSON(path, all)
}

func newGen() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// pollChanges returns changes since the stored position. With no stored position it starts
// tracking (recording the files already in the folder) and returns Initialized=true.
//
// A folder inside a shared drive, and a whole shared drive, are read from that drive's own
// change log: Google does not reliably report a member's shared-drive items in the user log.
func pollChanges(ctx context.Context, a *App, target feedTarget, reset bool) (poll, error) {
	c, err := a.Clients(ctx)
	if err != nil {
		return poll{}, err
	}
	folder := target.Folder
	// Attribute everything this poll records to the login its clients act for.
	owner := feed{Account: c.Account, Session: c.Session}
	all, err := loadFeeds(feedsFile(a.Dir, c))
	if err != nil {
		return poll{}, err
	}
	key := target.key()
	fd := all[key]
	if fd != nil && (fd.Account != owner.Account || fd.Session != owner.Session) {
		fd = nil // recorded under another login
	}
	if fd == nil || fd.Token == "" || reset {
		driveID := target.Drive
		if folder != "" {
			f, err := c.Drive.Files.Get(folder).Fields("id,mimeType,driveId").SupportsAllDrives(true).Context(ctx).Do()
			if err != nil {
				return poll{}, err
			}
			if f.MimeType != folderMime {
				return poll{}, apperr.Newf(apperr.Usage, "%s is not a folder", folder)
			}
			driveID = f.DriveId
		} else if driveID != "" {
			if _, err := c.Drive.Drives.Get(driveID).Fields("id").Context(ctx).Do(); err != nil {
				return poll{}, err
			}
		}
		// Folder membership is listed before and after taking the position. A file deleted
		// right after the position was taken is in the first listing; one created just before
		// it is in the second. Either way its later removal is recognised.
		known := map[string]bool{}
		if folder != "" {
			if known, err = folderTree(ctx, c.Drive, folder, driveID); err != nil {
				return poll{}, err
			}
		}
		stc := c.Drive.Changes.GetStartPageToken().SupportsAllDrives(true)
		if driveID != "" {
			stc = stc.DriveId(driveID)
		}
		st, err := stc.Context(ctx).Do()
		if err != nil {
			return poll{}, err
		}
		if folder != "" {
			again, err := folderTree(ctx, c.Drive, folder, driveID)
			if err != nil {
				return poll{}, err
			}
			for id := range again {
				known[id] = true
			}
		}
		next := &feed{Token: st.StartPageToken, Gen: newGen(), Account: owner.Account, Session: owner.Session,
			DriveID: driveID, Known: known}
		mode := start
		if reset {
			mode = restart
		}
		read := fd
		return poll{Changes: []Change{}, Initialized: true,
			Commit: func(ctx context.Context) error { return commitFeed(ctx, a, c, key, read, next, mode) }}, nil
	}
	read := &feed{Token: fd.Token, Gen: fd.Gen}
	if fd.Known == nil {
		fd.Known = map[string]bool{}
	}
	// A folder moved between My Drive and a shared drive since tracking started has its
	// changes in another change log; reading the old one would silently report nothing.
	if folder != "" {
		f, err := c.Drive.Files.Get(folder).Fields("id,driveId").SupportsAllDrives(true).Context(ctx).Do()
		if err != nil {
			return poll{}, err
		}
		if f.DriveId != fd.DriveID {
			return poll{}, apperr.Newf(apperr.FeedResetRequired, "folder %s moved between My Drive and a shared drive since tracking started; changes since then may be missing", folder).
				WithHint("Re-read the folder, then run `drivekey changes --folder " + folder + " --reset` to track it in its new place.")
		}
	}

	in := newAncestry(ctx, c.Drive, folder)
	byID := map[string]int{}
	out := []Change{}
	token := fd.Token
	for {
		call := c.Drive.Changes.List(token).IncludeRemoved(true).PageSize(1000).Spaces("drive").
			SupportsAllDrives(true).IncludeItemsFromAllDrives(true)
		if fd.DriveID != "" {
			call = call.DriveId(fd.DriveID)
		}
		resp, err := call.Fields("nextPageToken,newStartPageToken,changes(changeType,fileId,removed,time,file(id,name,mimeType,modifiedTime,parents,trashed))").
			Context(ctx).Do()
		if err != nil {
			return poll{}, err
		}
		for _, ch := range resp.Changes {
			if ch.FileId == "" {
				continue // a change to a shared drive itself, not to a file
			}
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
	return poll{Changes: out, Commit: func(ctx context.Context) error {
		fd.Token = token
		return commitFeed(ctx, a, c, key, read, fd, advance)
	}}, nil
}

// folderTree returns the ids of everything inside folder, at any depth. driveID is the shared
// drive the folder is in, or "" for My Drive.
func folderTree(ctx context.Context, d *drive.Service, folder, driveID string) (map[string]bool, error) {
	known := map[string]bool{}
	queue := []string{folder}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		err := inDrive(d.Files.List().Q(quoteQ(parent)+" in parents").PageSize(1000), driveID).
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
	f, err := a.drive.Files.Get(id).Fields("id,parents").SupportsAllDrives(true).Context(a.ctx).Do()
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
	const usage = "changes [--folder FOLDER_ID | --drive SHARED_DRIVE_ID] [--peek] [--reset]"
	fs := newFlags("changes")
	folder := fs.String("folder", "", "only report files inside this folder (any depth)")
	sharedDrive := fs.String("drive", "", "report changes in this shared drive")
	peek := fs.Bool("peek", false, "do not advance the stored position")
	reset := fs.Bool("reset", false, "forget the stored position and start tracking from now")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return nil, err
	}
	if err := positional(pos, 0, 0, usage); err != nil {
		return nil, err
	}
	target, err := newFeedTarget(*folder, *sharedDrive)
	if err != nil {
		return nil, err
	}
	p, err := pollChanges(ctx, a, target, *reset)
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
		if err := p.Commit(ctx); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// watchOnce polls one feed and commits only after every change was written. No lock is held
// while writing, so a stalled consumer cannot block other drivekey commands.
func watchOnce(ctx context.Context, a *App, target feedTarget) error {
	p, err := pollChanges(ctx, a, target, false)
	if err != nil {
		return err
	}
	for _, ch := range p.Changes {
		if err := writeCompactJSON(a.Stdout, ch); err != nil {
			return &deliveryError{err}
		}
	}
	return p.Commit(ctx)
}

// deliveryError means stdout is gone; watch stops instead of polling into the void.
type deliveryError struct{ err error }

func (e *deliveryError) Error() string { return "write changes: " + e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

func runWatch(ctx context.Context, a *App, args []string) (any, error) {
	const usage = "watch [--folder FOLDER_ID | --drive SHARED_DRIVE_ID] [--interval 60s]"
	fs := newFlags("watch")
	folder := fs.String("folder", "", "only report files inside this folder (any depth)")
	sharedDrive := fs.String("drive", "", "report changes in this shared drive")
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
	target, err := newFeedTarget(*folder, *sharedDrive)
	if err != nil {
		return nil, err
	}
	for {
		if err := watchOnce(ctx, a, target); err != nil {
			if ctx.Err() != nil {
				return nil, nil
			}
			var de *deliveryError
			if errors.As(err, &de) {
				return nil, err
			}
			e := apperr.As(mapErr(err))
			switch e.Code {
			case apperr.NotLoggedIn, apperr.NotSetUp, apperr.Usage, apperr.NotFound, apperr.GcloudMissing,
				apperr.ProfileRequired, apperr.ProfileNotFound, apperr.RelayMode, apperr.RelayRouteRefused,
				apperr.FeedResetRequired:
				// Retrying cannot fix these. Relay denials and logins can be fixed by the
				// owner while watch runs, so those are reported and retried.
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
