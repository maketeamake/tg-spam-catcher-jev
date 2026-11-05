package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/events/mocks"
	"github.com/umputun/tg-spam/app/profile"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

type fakeProfile struct {
	cleared  bool
	hasOpen  bool
	resolved []int64
	first    bool
	userID   int64
}

func (f *fakeProfile) Check(context.Context, profile.Event) profile.Result { return profile.Result{} }
func (f *fakeProfile) RememberPost(int, string)                            {}
func (f *fakeProfile) PostText(int) string                                 { return "" }
func (f *fakeProfile) Cleared(context.Context, int64, time.Time) bool      { return f.cleared }
func (f *fakeProfile) HasOpenReview(context.Context, int64, int64) bool    { return f.hasOpen }
func (f *fakeProfile) Resolve(_ context.Context, id, _ int64, _ profile.Verdict) (profile.Record, bool, error) {
	f.resolved = append(f.resolved, id)
	return profile.Record{ID: id, UserID: f.userID}, f.first, nil
}

type profileEnv struct {
	l       *TelegramListener
	api     *mocks.TbAPIMock
	bot     *mocks.BotMock
	prof    *fakeProfile
	spamLog int
}

func newProfileEnv(t *testing.T) *profileEnv {
	t.Helper()
	env := &profileEnv{prof: &fakeProfile{first: true, userID: 42}}
	env.api = &mocks.TbAPIMock{
		SendFunc:    func(tbapi.Chattable) (tbapi.Message, error) { return tbapi.Message{}, nil },
		RequestFunc: func(tbapi.Chattable) (*tbapi.APIResponse, error) { return &tbapi.APIResponse{Ok: true}, nil },
		GetChatFunc: func(tbapi.ChatInfoConfig) (tbapi.ChatFullInfo, error) { return tbapi.ChatFullInfo{}, nil },
	}
	env.bot = &mocks.BotMock{
		RemoveApprovedUserFunc: func(int64) error { return nil },
		AddApprovedUserFunc:    func(int64, string) error { return nil },
		IsApprovedUserFunc:     func(int64) bool { return false },
	}
	env.l = &TelegramListener{
		TbAPI: env.api, Bot: env.bot, Profile: env.prof,
		Locator:    &mocks.LocatorMock{AddSpamFunc: func(context.Context, int64, []spamcheck.Response) error { return nil }},
		SpamLogger: SpamLoggerFunc(func(*bot.Message, *bot.Response) { env.spamLog++ }),
		chatID:     -1001, adminChatID: -2002,
	}
	return env
}

func commentEvent() profile.Event {
	return profile.Event{Kind: profile.KindComment, User: profile.User{ID: 42, FirstName: "Eva", Username: "eva"},
		ChatID: -1001, MessageID: 77, Comment: "check my <b>profile</b>"}
}

// requestTypes lists the kinds of Request calls, to assert what was sent to Telegram
func (e *profileEnv) requestTypes() []string {
	var res []string
	for _, c := range e.api.RequestCalls() {
		switch c.C.(type) {
		case tbapi.DeleteMessageConfig:
			res = append(res, "delete")
		case tbapi.BanChatMemberConfig:
			res = append(res, "ban")
		case tbapi.RestrictChatMemberConfig:
			res = append(res, "restrict")
		case tbapi.UnbanChatMemberConfig:
			res = append(res, "unban")
		default:
			res = append(res, "other")
		}
	}
	return res
}

// cardButtons returns the callback data of the last card sent to the admin chat
func (e *profileEnv) cardButtons(t *testing.T) []string {
	t.Helper()
	calls := e.api.SendCalls()
	require.NotEmpty(t, calls, "no card sent")
	msg, ok := calls[len(calls)-1].C.(tbapi.MessageConfig)
	require.True(t, ok, "last send is not a message")
	assert.Equal(t, int64(-2002), msg.ChatID)
	kb, ok := msg.ReplyMarkup.(tbapi.InlineKeyboardMarkup)
	require.True(t, ok)
	var res []string
	for _, b := range kb.InlineKeyboard[0] {
		res = append(res, *b.CallbackData)
	}
	return res
}

func TestApplyProfile(t *testing.T) {
	ban := profile.Result{DecisionID: 5, Action: profile.ActionBan, Reason: "r"}
	review := profile.Result{DecisionID: 6, Action: profile.ActionReview, Reason: "r"}

	t.Run("ban deletes, bans and sends a card with unban", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.applyProfile(context.Background(), commentEvent(), ban, time.Now())
		assert.Equal(t, []string{"delete", "ban"}, env.requestTypes())
		assert.Equal(t, []string{"Pu5"}, env.cardButtons(t))
		assert.Len(t, env.bot.RemoveApprovedUserCalls(), 1)
		assert.Equal(t, 1, env.spamLog)
		card := env.api.SendCalls()[0].C.(tbapi.MessageConfig).Text
		assert.Contains(t, card, "check my &lt;b&gt;profile&lt;/b&gt;", "user text must be escaped")
	})

	t.Run("second ban of a burst sends no second card", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.applyProfile(context.Background(), commentEvent(), ban, time.Now())
		env.l.applyProfile(context.Background(), commentEvent(), ban, time.Now())
		assert.Len(t, env.api.SendCalls(), 1, "one card")
		assert.Equal(t, []string{"delete", "ban", "delete", "ban"}, env.requestTypes(), "every comment is still deleted")
	})

	t.Run("review deletes, restricts with empty permissions and sends ban and not spam", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.applyProfile(context.Background(), commentEvent(), review, time.Now())
		assert.Equal(t, []string{"delete", "restrict"}, env.requestTypes())
		r := env.api.RequestCalls()[1].C.(tbapi.RestrictChatMemberConfig)
		assert.Equal(t, &tbapi.ChatPermissions{}, r.Permissions)
		assert.Equal(t, []string{"Pb6", "Ph6"}, env.cardButtons(t))
		assert.Len(t, env.bot.RemoveApprovedUserCalls(), 1, "a flagged user is not trusted any more")
	})

	t.Run("open review suppresses a second card but still restricts", func(t *testing.T) {
		env := newProfileEnv(t)
		env.prof.hasOpen = true
		env.l.applyProfile(context.Background(), commentEvent(), review, time.Now())
		assert.Equal(t, []string{"delete", "restrict"}, env.requestTypes())
		assert.Empty(t, env.api.SendCalls())
	})

	t.Run("dry run and training make no mutating calls but still send cards", func(t *testing.T) {
		for _, mode := range []string{"dry", "training"} {
			for _, res := range []profile.Result{ban, review} {
				env := newProfileEnv(t)
				env.l.Dry, env.l.TrainingMode = mode == "dry", mode == "training"
				env.l.applyProfile(context.Background(), commentEvent(), res, time.Now())
				assert.Empty(t, env.api.RequestCalls(), "%s %s", mode, res.Action)
				require.Len(t, env.api.SendCalls(), 1, "%s %s", mode, res.Action)
				assert.Contains(t, env.api.SendCalls()[0].C.(tbapi.MessageConfig).Text, mode)
			}
		}
	})

	t.Run("user cleared by an admin meanwhile is left alone", func(t *testing.T) {
		env := newProfileEnv(t)
		env.prof.cleared = true
		env.l.applyProfile(context.Background(), commentEvent(), ban, time.Now())
		assert.Empty(t, env.api.RequestCalls())
		assert.Empty(t, env.api.SendCalls())
	})

	t.Run("skip and pass do nothing", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.applyProfile(context.Background(), commentEvent(), profile.Result{Action: profile.ActionSkip}, time.Now())
		env.l.applyProfile(context.Background(), commentEvent(), profile.Result{Action: profile.ActionPass}, time.Now())
		assert.Empty(t, env.api.RequestCalls())
		assert.Empty(t, env.api.SendCalls())
	})

	t.Run("reaction has no message to delete", func(t *testing.T) {
		env := newProfileEnv(t)
		ev := commentEvent()
		ev.Kind, ev.MessageID, ev.Comment = profile.KindReaction, 0, ""
		env.l.applyProfile(context.Background(), ev, ban, time.Now())
		assert.Equal(t, []string{"ban"}, env.requestTypes())
	})

	t.Run("review without admin group does nothing", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.adminChatID = 0
		env.l.applyProfile(context.Background(), commentEvent(), review, time.Now())
		assert.Empty(t, env.api.RequestCalls())
	})
}

func TestHandleProfileCallback(t *testing.T) {
	callback := func(data string, chat int64) *tbapi.CallbackQuery {
		return &tbapi.CallbackQuery{ID: "cb", Data: data, From: &tbapi.User{ID: 1, UserName: "admin"},
			Message: &tbapi.Message{MessageID: 9, Text: "card", Chat: tbapi.Chat{ID: chat}}}
	}

	t.Run("not spam lifts the restriction and approves", func(t *testing.T) {
		env := newProfileEnv(t)
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback("Ph6", -2002)))
		assert.Equal(t, []int64{6}, env.prof.resolved)
		assert.Contains(t, env.requestTypes(), "restrict")
		assert.NotContains(t, env.requestTypes(), "unban")
		require.Len(t, env.bot.AddApprovedUserCalls(), 1)
		assert.Equal(t, int64(42), env.bot.AddApprovedUserCalls()[0].ID)
	})

	t.Run("unban unbans only if banned in the group and the channel", func(t *testing.T) {
		env := newProfileEnv(t)
		env.l.linkedChannelID = -1003
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback("Pu5", -2002)))
		var unbans []tbapi.UnbanChatMemberConfig
		for _, c := range env.api.RequestCalls() {
			if u, ok := c.C.(tbapi.UnbanChatMemberConfig); ok {
				unbans = append(unbans, u)
			}
		}
		require.Len(t, unbans, 2)
		for _, u := range unbans {
			assert.True(t, u.OnlyIfBanned)
		}
	})

	t.Run("ban button bans for real", func(t *testing.T) {
		env := newProfileEnv(t)
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback("Pb6", -2002)))
		assert.Contains(t, env.requestTypes(), "ban")
	})

	t.Run("second admin click does nothing", func(t *testing.T) {
		env := newProfileEnv(t)
		env.prof.first = false
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback("Ph6", -2002)))
		assert.NotContains(t, env.requestTypes(), "restrict")
		assert.Empty(t, env.bot.AddApprovedUserCalls())
		edit := env.api.SendCalls()[0].C.(tbapi.EditMessageTextConfig)
		assert.True(t, strings.Contains(edit.Text, "already answered"), edit.Text)
	})

	t.Run("failed action keeps the buttons and the same button repeats it", func(t *testing.T) {
		env := newProfileEnv(t)
		fail := true
		env.api.RequestFunc = func(c tbapi.Chattable) (*tbapi.APIResponse, error) {
			if _, ok := c.(tbapi.RestrictChatMemberConfig); ok && fail {
				return nil, errors.New("telegram is down")
			}
			return &tbapi.APIResponse{Ok: true}, nil
		}
		q := callback("Ph6", -2002)
		q.Message.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{{{Text: "Not spam"}}}}
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), q))
		edit := env.api.SendCalls()[0].C.(tbapi.EditMessageTextConfig)
		assert.Contains(t, edit.Text, "press again to retry")
		assert.NotEmpty(t, edit.ReplyMarkup.InlineKeyboard, "buttons stay")

		fail, env.prof.first = false, false // the verdict is recorded, the second click is not the first
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), q))
		assert.Len(t, env.bot.AddApprovedUserCalls(), 2, "the retry ran the release again")
		edit = env.api.SendCalls()[1].C.(tbapi.EditMessageTextConfig)
		assert.Empty(t, edit.ReplyMarkup.InlineKeyboard, "buttons are gone after success")

		require.NoError(t, env.l.HandleProfileCallback(context.Background(), q))
		assert.Len(t, env.bot.AddApprovedUserCalls(), 2, "nothing is repeated after success")
	})

	t.Run("dry run buttons change nothing in telegram", func(t *testing.T) {
		for _, data := range []string{"Pb6", "Ph6", "Pu6"} {
			env := newProfileEnv(t)
			env.l.Dry = true
			require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback(data, -2002)))
			for _, typ := range env.requestTypes() {
				assert.Equal(t, "other", typ, data) // only the callback answer
			}
		}
	})

	t.Run("callback from another chat is ignored", func(t *testing.T) {
		env := newProfileEnv(t)
		require.NoError(t, env.l.HandleProfileCallback(context.Background(), callback("Pb6", -999)))
		assert.Empty(t, env.prof.resolved)
		assert.Empty(t, env.api.RequestCalls())
	})
}

func TestProfileQueueOverflowDoesNotBlock(t *testing.T) {
	l := &TelegramListener{Profile: &fakeProfile{}}
	l.profileJobs = make(chan func(context.Context), 1)
	done := make(chan struct{})
	go func() {
		for range 5 {
			l.enqueueProfile(func(context.Context) {})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}
	assert.Len(t, l.profileJobs, 1)
}

func TestProfileEligible(t *testing.T) {
	env := newProfileEnv(t)
	m := &tbapi.Message{From: &tbapi.User{ID: 42}}
	msg := &bot.Message{From: bot.User{ID: 42, Username: "eva"}}
	assert.True(t, env.l.profileEligible(m, msg, -1001))

	assert.False(t, env.l.profileEligible(m, msg, -555), "other chat")
	m.From.IsBot = true
	assert.False(t, env.l.profileEligible(m, msg, -1001), "bot")
	m.From.IsBot = false
	env.bot.IsApprovedUserFunc = func(int64) bool { return true }
	assert.False(t, env.l.profileEligible(m, msg, -1001), "approved users never reach the service")
	env.l.Profile = nil
	env.bot.IsApprovedUserFunc = func(int64) bool { return false }
	assert.False(t, env.l.profileEligible(m, msg, -1001), "feature off")
}
