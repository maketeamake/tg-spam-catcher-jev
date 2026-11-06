package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	log "github.com/go-pkgz/lgr"
)

const (
	maxBioRunes         = 500
	maxChannelDescRunes = 500
	maxChannelPostRunes = 300
	maxPhotoBytes       = 8 << 20

	// jev accepts images with each side in [280, 1568] and total pixels in [78400, 2458624]
	minPhotoSide   = 280
	maxPhotoSide   = 1568
	minPhotoPixels = 78400
	maxPhotoPixels = 2458624
	wantPhotoSide  = 640
)

// TG is the subset of the Telegram bot API the dossier needs. *tbapi.BotAPI satisfies it.
type TG interface {
	GetChat(config tbapi.ChatInfoConfig) (tbapi.ChatFullInfo, error)
	GetUserProfilePhotos(config tbapi.UserProfilePhotosConfig) (tbapi.UserProfilePhotos, error)
	GetFileDirectURL(fileID string) (string, error)
	GetUserPersonalChatMessages(config tbapi.UserPersonalChatMessagesConfig) ([]tbapi.Message, error)
}

// PhotoRef points to one avatar by Telegram file id. The picture itself is never stored.
type PhotoRef struct {
	FileID   string `json:"file_id"`
	UniqueID string `json:"unique_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

// Channel is the personal channel shown in a profile
type Channel struct {
	Title       string   `json:"title,omitempty"`
	Username    string   `json:"username,omitempty"`
	Description string   `json:"description,omitempty"`
	LastPosts   []string `json:"last_posts,omitempty"`
}

// Dossier is everything visible in a user profile. Fields the bot could not read stay empty.
type Dossier struct {
	User      User       `json:"user"`
	Bio       string     `json:"bio,omitempty"`
	Channel   *Channel   `json:"personal_channel,omitempty"`
	Photos    []PhotoRef `json:"photos,omitempty"`
	FetchedAt time.Time  `json:"fetched_at"`
	Partial   bool       `json:"partial,omitempty"` // a part could not be read, the dossier is not cached
}

// Empty is true when the profile has nothing for the model to judge: no bio, channel or avatar
func (d *Dossier) Empty() bool { return d.Bio == "" && d.Channel == nil && len(d.Photos) == 0 }

// Fingerprint changes when anything the model sees in the profile changes
func (d *Dossier) Fingerprint() string {
	h := sha256.New()
	fmt.Fprint(h, d.User.FirstName, "|", d.User.LastName, "|", d.User.Username, "|", d.Bio)
	if d.Channel != nil {
		fmt.Fprint(h, "|", d.Channel.Title, d.Channel.Username, d.Channel.Description, strings.Join(d.Channel.LastPosts, "\x00"))
	}
	for _, p := range d.Photos {
		fmt.Fprint(h, "|", p.UniqueID)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PhotoIDs lists unique ids of the avatars
func (d *Dossier) PhotoIDs() []string {
	ids := make([]string, 0, len(d.Photos))
	for _, p := range d.Photos {
		ids = append(ids, p.UniqueID)
	}
	return ids
}

// collector builds dossiers from Telegram
type collector struct {
	tg     TG
	http   HTTPDoer
	photos int
	posts  int
}

// fetch reads the profile. A part that cannot be read is skipped, the dossier is then partial.
func (c *collector) fetch(ctx context.Context, u User) *Dossier {
	d := &Dossier{User: u, FetchedAt: time.Now()}

	info, err := callCtx(ctx, func() (tbapi.ChatFullInfo, error) {
		return c.tg.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: u.ID}})
	})
	if err != nil {
		log.Printf("[DEBUG] profile: can't get chat info of %d: %v", u.ID, err)
		d.Partial = true
	} else {
		d.Bio = trunc(info.Bio, maxBioRunes)
		if pc := info.PersonalChat; pc != nil {
			d.Channel = c.fetchChannel(ctx, d, pc)
		}
	}

	if c.photos > 0 {
		photos, err := callCtx(ctx, func() (tbapi.UserProfilePhotos, error) {
			return c.tg.GetUserProfilePhotos(tbapi.UserProfilePhotosConfig{UserID: u.ID, Limit: c.photos})
		})
		if err != nil {
			log.Printf("[DEBUG] profile: can't get photos of %d: %v", u.ID, err)
			d.Partial = true
		}
		for _, sizes := range photos.Photos {
			if best, ok := pickSize(sizes); ok {
				d.Photos = append(d.Photos, PhotoRef{FileID: best.FileID, UniqueID: best.FileUniqueID, Width: best.Width,
					Height: best.Height})
			}
		}
	}
	return d
}

func (c *collector) fetchChannel(ctx context.Context, d *Dossier, pc *tbapi.Chat) *Channel {
	ch := &Channel{Title: pc.Title, Username: pc.UserName}
	info, err := callCtx(ctx, func() (tbapi.ChatFullInfo, error) {
		return c.tg.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: pc.ID}})
	})
	if err != nil {
		d.Partial = true
	} else {
		ch.Description = trunc(info.Description, maxChannelDescRunes)
	}
	if c.posts > 0 {
		msgs, err := callCtx(ctx, func() ([]tbapi.Message, error) {
			return c.tg.GetUserPersonalChatMessages(tbapi.UserPersonalChatMessagesConfig{UserID: d.User.ID, Limit: c.posts})
		})
		if err != nil {
			log.Printf("[DEBUG] profile: can't get personal channel posts of %d: %v", d.User.ID, err)
			d.Partial = true
		}
		for _, m := range msgs {
			text := m.Text
			if text == "" {
				text = m.Caption
			}
			if text = strings.TrimSpace(text); text != "" {
				ch.LastPosts = append(ch.LastPosts, trunc(text, maxChannelPostRunes))
			}
		}
	}
	return ch
}

// stillFresh tells if a cached dossier still matches the user: same names, bio, personal channel and first avatar.
// Posts of the personal channel are not compared, they are read again when the cache expires.
func (c *collector) stillFresh(ctx context.Context, d *Dossier, u User) bool {
	if d.User.FirstName != u.FirstName || d.User.LastName != u.LastName || d.User.Username != u.Username {
		return false
	}
	info, err := callCtx(ctx, func() (tbapi.ChatFullInfo, error) {
		return c.tg.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: u.ID}})
	})
	if err == nil && (trunc(info.Bio, maxBioRunes) != d.Bio || channelChanged(info.PersonalChat, d.Channel)) {
		return false
	}
	photos, err := callCtx(ctx, func() (tbapi.UserProfilePhotos, error) {
		return c.tg.GetUserProfilePhotos(tbapi.UserProfilePhotosConfig{UserID: u.ID, Limit: 1})
	})
	if err != nil {
		return true // can't tell, a cached dossier is better than none
	}
	if len(photos.Photos) == 0 {
		return len(d.Photos) == 0
	}
	best, ok := pickSize(photos.Photos[0])
	if !ok {
		return true // the first avatar is unusable either way
	}
	return len(d.Photos) > 0 && d.Photos[0].UniqueID == best.FileUniqueID
}

func channelChanged(pc *tbapi.Chat, ch *Channel) bool {
	if pc == nil || ch == nil {
		return (pc == nil) != (ch == nil)
	}
	return pc.Title != ch.Title || pc.UserName != ch.Username
}

// download loads the avatars into memory. Pictures that can't be loaded are skipped.
func (c *collector) download(ctx context.Context, refs []PhotoRef) [][]byte {
	var out [][]byte
	for _, ref := range refs {
		data, err := c.downloadOne(ctx, ref.FileID)
		if err != nil {
			log.Printf("[DEBUG] profile: can't download photo %s: %v", ref.UniqueID, err)
			continue
		}
		out = append(out, data)
	}
	return out
}

func (c *collector) downloadOne(ctx context.Context, fileID string) ([]byte, error) {
	link, err := callCtx(ctx, func() (string, error) { return c.tg.GetFileDirectURL(fileID) })
	if err != nil {
		return nil, fmt.Errorf("can't get file url: %w", errNoURL(err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
	if err != nil {
		return nil, errors.New("can't make download request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", errNoURL(err))
	}
	defer resp.Body.Close() //nolint:errcheck // nothing useful to do with a close error on a read body
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("download read failed: %w", errNoURL(err))
	}
	if len(data) > maxPhotoBytes {
		return nil, errors.New("photo is too large")
	}
	return data, nil
}

// errNoURL strips the request url from a transport error: the file url contains the bot token
func errNoURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// pickSize chooses the size of a photo that jev accepts and is the closest to 640 px
func pickSize(sizes []tbapi.PhotoSize) (tbapi.PhotoSize, bool) {
	var best tbapi.PhotoSize
	found := false
	dist := func(s tbapi.PhotoSize) int { return absInt(max(s.Width, s.Height) - wantPhotoSide) }
	for _, s := range sizes {
		px := s.Width * s.Height
		if s.Width < minPhotoSide || s.Height < minPhotoSide || s.Width > maxPhotoSide || s.Height > maxPhotoSide ||
			px < minPhotoPixels || px > maxPhotoPixels {
			continue
		}
		if !found || dist(s) < dist(best) {
			best, found = s, true
		}
	}
	return best, found
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// callCtx runs a Telegram call that takes no context and gives up waiting when the context ends.
// The abandoned call finishes in the background.
func callCtx[T any](ctx context.Context, f func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := f()
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("telegram call abandoned: %w", ctx.Err())
	}
}

func trunc(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}
