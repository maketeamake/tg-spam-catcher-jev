package events

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	log "github.com/go-pkgz/lgr"

	"github.com/umputun/tg-spam/app/bot"
	"github.com/umputun/tg-spam/app/profile"
	"github.com/umputun/tg-spam/lib/spamcheck"
)

// profile card buttons: P + action letter + decision id. The letter P is not used by any other callback.
const (
	profileCallbackPrefix = "P"
	profileBanBtn         = "Pb"
	profileHamBtn         = "Ph"
	profileUnbanBtn       = "Pu"
)

// profile queue defaults
const (
	defaultProfileWorkers = 64
	profileQueueSize      = 1000
)

// ProfileChecker is the profile check service
type ProfileChecker interface {
	Check(ctx context.Context, ev profile.Event) profile.Result
	RememberPost(msgID int, text string)
	PostText(msgID int) string
	Resolve(ctx context.Context, decisionID, adminID int64, v profile.Verdict) (profile.Record, bool, error)
	Cleared(ctx context.Context, userID int64, since time.Time) bool
	HasOpenReview(ctx context.Context, userID, beforeDecisionID int64) bool
}

// startProfileWorkers runs the pool that executes profile checks, so the update loop never waits for jev
func (l *TelegramListener) startProfileWorkers(ctx context.Context) {
	if l.Profile == nil {
		return
	}
	workers := l.ProfileWorkers
	if workers <= 0 {
		workers = defaultProfileWorkers
	}
	l.profileJobs = make(chan func(context.Context), profileQueueSize)
	for range workers {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-l.profileJobs:
					job(ctx)
				}
			}
		}()
	}
	log.Printf("[INFO] profile check enabled, %d workers", workers)
}

// enqueueProfile never blocks: when the queue is full the event is dropped and logged
func (l *TelegramListener) enqueueProfile(job func(context.Context)) {
	select {
	case l.profileJobs <- job:
	default:
		log.Printf("[WARN] profile queue is full, event is not checked")
	}
}

// profileEligible tells if a message author must be checked: a human, not approved, not a superuser
func (l *TelegramListener) profileEligible(m *tbapi.Message, msg *bot.Message, fromChat int64) bool {
	return l.Profile != nil && fromChat == l.chatID && msg.SenderChat.ID == 0 && msg.From.ID != 0 &&
		m.From != nil && !m.From.IsBot && !l.SuperUsers.IsSuper(msg.From.Username, msg.From.ID) && !l.Bot.IsApprovedUser(msg.From.ID)
}

func profileUserOf(u *tbapi.User) profile.User {
	return profile.User{ID: u.ID, FirstName: strings.TrimSpace(u.FirstName), LastName: strings.TrimSpace(u.LastName),
		Username: u.UserName, IsPremium: u.IsPremium}
}

func botUserOf(u profile.User) bot.User {
	res := bot.User{ID: u.ID, Username: u.Username, FirstName: u.FirstName, LastName: u.LastName, IsPremium: u.IsPremium}
	res.DisplayName = strings.TrimSpace(u.FirstName + " " + u.LastName)
	return res
}

// profileComment queues the profile check of a comment tg-spam found clean
func (l *TelegramListener) profileComment(m *tbapi.Message, msg *bot.Message) {
	post, reply := l.postContext(m)
	ev := profile.Event{Kind: profile.KindComment, User: profileUserOf(m.From), ChatID: m.Chat.ID, MessageID: m.MessageID,
		PostText: post, Comment: msg.Text, ReplyText: reply}
	l.queueProfileEvent(ev)
}

// postContext finds the channel post a comment belongs to and the text it replies to
func (l *TelegramListener) postContext(m *tbapi.Message) (post, reply string) {
	if m.MessageThreadID != 0 {
		post = l.Profile.PostText(m.MessageThreadID)
	}
	if rm := m.ReplyToMessage; rm != nil {
		text := rm.Text
		if text == "" {
			text = rm.Caption
		}
		switch {
		case rm.IsAutomaticForward:
			if post == "" {
				post = text
			}
		case rm.MessageID != m.MessageThreadID:
			reply = text
		}
	}
	return post, reply
}

// profileReaction queues the profile check of a user who reacted and was not flagged by the reaction counter
func (l *TelegramListener) profileReaction(r *tbapi.MessageReactionUpdated) {
	if l.Profile == nil || r.User == nil || r.User.IsBot || l.Bot.IsApprovedUser(r.User.ID) {
		return
	}
	l.queueProfileEvent(profile.Event{Kind: profile.KindReaction, User: profileUserOf(r.User), ChatID: r.Chat.ID, MessageID: r.MessageID})
}

// profileJoin queues the CAS (and optionally profile) check of a new member
func (l *TelegramListener) profileJoin(member tbapi.User) {
	if l.Profile == nil || member.IsBot || l.Bot.IsApprovedUser(member.ID) {
		return
	}
	l.queueProfileEvent(profile.Event{Kind: profile.KindJoin, User: profileUserOf(&member), ChatID: l.chatID})
}

func (l *TelegramListener) queueProfileEvent(ev profile.Event) {
	started := time.Now()
	l.enqueueProfile(func(ctx context.Context) {
		res := l.Profile.Check(ctx, ev)
		l.applyProfile(ctx, ev, res, started)
	})
}

// applyProfile executes the decision unless an admin cleared the user while the check was running
func (l *TelegramListener) applyProfile(ctx context.Context, ev profile.Event, res profile.Result, started time.Time) {
	log.Printf("[DEBUG] profile %s check of %d: %s (%s), %v", ev.Kind, ev.User.ID, res.Action, res.Reason, res.Latency)
	if res.Action == profile.ActionSkip {
		log.Printf("[WARN] profile check of %d was not completed: %v", ev.User.ID, res.Err)
		return
	}
	if res.Action != profile.ActionBan && res.Action != profile.ActionReview {
		return
	}
	if l.Profile.Cleared(ctx, ev.User.ID, started) {
		log.Printf("[INFO] profile %s of %d dropped, an admin marked the user not spam meanwhile", res.Action, ev.User.ID)
		return
	}
	if res.Action == profile.ActionBan {
		l.profileBan(ctx, ev, res)
		return
	}
	l.profileReview(ctx, ev, res)
}

func (l *TelegramListener) profileBan(ctx context.Context, ev profile.Event, res profile.Result) {
	user := botUserOf(ev.User)
	userStr := user.String()
	if err := l.Bot.RemoveApprovedUser(ev.User.ID); err != nil {
		log.Printf("[DEBUG] profile: user %d was not in the approved list: %v", ev.User.ID, err)
	}

	var failures []string
	if l.deleteProfileMessage(ev) != nil {
		failures = append(failures, "delete message")
	}
	if err := banUserOrChannel(banRequest{duration: bot.PermanentBanDuration, userID: ev.User.ID, userName: userStr,
		chatID: l.chatID, dry: l.Dry, training: l.TrainingMode, tbAPI: l.TbAPI, restrict: l.SoftBanMode}); err != nil {
		log.Printf("[WARN] profile: failed to ban %s: %v", userStr, err)
		failures = append(failures, "ban in group")
	}
	if l.linkedChannelID != 0 && !l.SoftBanMode {
		if err := banUserOrChannel(banRequest{duration: bot.PermanentBanDuration, userID: ev.User.ID, userName: userStr,
			chatID: l.linkedChannelID, dry: l.Dry, training: l.TrainingMode, tbAPI: l.TbAPI}); err != nil {
			log.Printf("[WARN] profile: failed to ban %s in the channel: %v", userStr, err)
			failures = append(failures, "ban in channel")
		}
	}

	// a burst of comments is checked in parallel and each result bans and deletes, but one card is enough
	at, dup := l.profileBanned.LoadOrStore(ev.User.ID, time.Now())
	if dup && time.Since(at.(time.Time)) < time.Minute {
		return
	}
	l.profileBanned.Store(ev.User.ID, time.Now())

	checks := []spamcheck.Response{{Name: "profile", Spam: true, Details: res.Reason}}
	if err := l.Locator.AddSpam(ctx, ev.User.ID, checks); err != nil {
		log.Printf("[WARN] profile: failed to add spam to locator: %v", err)
	}
	text := ev.Comment
	if text == "" {
		text = "[profile " + string(ev.Kind) + "]"
	}
	l.SpamLogger.Save(&bot.Message{ID: ev.MessageID, From: user, Text: text},
		&bot.Response{Send: true, BanInterval: bot.PermanentBanDuration, User: user, CheckResults: checks})

	if l.AggressiveCleanup && !l.Dry && !l.TrainingMode && l.adminHandler != nil {
		go func() {
			if n, err := l.adminHandler.deleteUserMessages(ev.User.ID); err != nil {
				log.Printf("[WARN] profile: aggressive cleanup of %d failed after %d messages: %v", ev.User.ID, n, err)
			}
		}()
	}

	l.sendProfileCard(ev, res, l.cardTitle("banned"), failures, [][2]string{{"Unban", profileUnbanBtn}})
}

func (l *TelegramListener) profileReview(ctx context.Context, ev profile.Event, res profile.Result) {
	if l.adminChatID == 0 {
		log.Printf("[WARN] profile: review of %d needs an admin group, nothing done", ev.User.ID)
		return
	}
	// tg-spam approves a user whose clean first comments pass, a flagged user must be checked again
	if err := l.Bot.RemoveApprovedUser(ev.User.ID); err != nil {
		log.Printf("[DEBUG] profile: user %d was not in the approved list: %v", ev.User.ID, err)
	}
	var failures []string
	if l.deleteProfileMessage(ev) != nil {
		failures = append(failures, "delete message")
	}
	if !l.Dry && !l.TrainingMode {
		// an empty permission set also takes away the right to react
		_, err := l.TbAPI.Request(tbapi.RestrictChatMemberConfig{
			ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: l.chatID}, UserID: ev.User.ID},
			Permissions:      &tbapi.ChatPermissions{},
		})
		if err != nil {
			log.Printf("[WARN] profile: failed to restrict %d: %v", ev.User.ID, err)
			failures = append(failures, "restrict")
		}
	}
	if l.Profile.HasOpenReview(ctx, ev.User.ID, res.DecisionID) {
		log.Printf("[DEBUG] profile: user %d already has a review card, no new one", ev.User.ID)
		return
	}
	l.sendProfileCard(ev, res, l.cardTitle("needs review"), failures, [][2]string{{"Ban", profileBanBtn}, {"Not spam", profileHamBtn}})
}

// deleteProfileMessage removes the flagged comment. Reactions and joins have nothing to delete.
func (l *TelegramListener) deleteProfileMessage(ev profile.Event) error {
	if ev.Kind != profile.KindComment || ev.MessageID == 0 || l.Dry || l.TrainingMode {
		return nil
	}
	_, err := l.TbAPI.Request(tbapi.DeleteMessageConfig{BaseChatMessage: tbapi.BaseChatMessage{
		MessageID: ev.MessageID, ChatConfig: tbapi.ChatConfig{ChatID: ev.ChatID}}})
	if err != nil {
		log.Printf("[WARN] profile: failed to delete message %d: %v", ev.MessageID, err)
	}
	return err
}

func (l *TelegramListener) cardTitle(what string) string {
	switch {
	case l.TrainingMode:
		return "[training] profile check: would have marked " + what
	case l.Dry:
		return "[dry run] profile check: would have marked " + what
	}
	return "profile check: " + what
}

// sendProfileCard posts the avatar and the card with buttons to the admin chat
func (l *TelegramListener) sendProfileCard(ev profile.Event, res profile.Result, title string, failures []string, buttons [][2]string) {
	if l.adminChatID == 0 {
		return
	}
	if d := res.Dossier; d != nil && len(d.Photos) > 0 {
		photo := tbapi.NewPhoto(l.adminChatID, tbapi.FileID(d.Photos[0].FileID))
		photo.Caption = "avatar of " + strconv.FormatInt(ev.User.ID, 10)
		if _, err := l.TbAPI.Send(photo); err != nil {
			log.Printf("[DEBUG] profile: can't send avatar to admin chat: %v", err)
		}
	}

	row := make([]tbapi.InlineKeyboardButton, 0, len(buttons))
	for _, b := range buttons {
		row = append(row, tbapi.NewInlineKeyboardButtonData(b[0], fmt.Sprintf("%s%d", b[1], res.DecisionID)))
	}
	msg := tbapi.NewMessage(l.adminChatID, l.cardText(ev, res, title, failures))
	msg.ParseMode = tbapi.ModeHTML
	msg.LinkPreviewOptions = tbapi.LinkPreviewOptions{IsDisabled: true}
	msg.ReplyMarkup = tbapi.NewInlineKeyboardMarkup(row)
	if _, err := l.TbAPI.Send(msg); err != nil {
		log.Printf("[WARN] profile: failed to send card as html, sending plain text: %v", err)
		msg.ParseMode = ""
		msg.Text = html.UnescapeString(stripTags(msg.Text))
		if _, err = l.TbAPI.Send(msg); err != nil {
			log.Printf("[WARN] profile: failed to send card: %v", err)
		}
	}
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cardText renders the admin card. Everything that comes from a user is escaped.
func (l *TelegramListener) cardText(ev profile.Event, res profile.Result, title string, failures []string) string {
	esc := html.EscapeString
	name := strings.TrimSpace(ev.User.FirstName + " " + ev.User.LastName)
	if name == "" {
		name = strconv.FormatInt(ev.User.ID, 10)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\nevent: %s\nuser: <a href=\"tg://user?id=%d\">%s</a>", esc(title), ev.Kind, ev.User.ID, esc(name))
	if ev.User.Username != "" {
		fmt.Fprintf(&b, " @%s", esc(ev.User.Username))
	}
	fmt.Fprintf(&b, " (%d)\n", ev.User.ID)
	if link := messageLink(ev.ChatID, ev.MessageID); link != "" {
		fmt.Fprintf(&b, "message: %s\n", esc(link))
	}
	if ev.Comment != "" {
		fmt.Fprintf(&b, "comment: %s\n", esc(truncateString(ev.Comment, 300, "...")))
	}
	if d := res.Dossier; d != nil {
		if d.Bio != "" {
			fmt.Fprintf(&b, "bio: %s\n", esc(truncateString(d.Bio, 300, "...")))
		}
		if d.Channel != nil && d.Channel.Title != "" {
			fmt.Fprintf(&b, "channel: %s\n", esc(truncateString(d.Channel.Title, 100, "...")))
		}
	}
	if res.CASHit {
		b.WriteString("CAS: listed\n")
	} else {
		fmt.Fprintf(&b, "jev: %s\n", esc(res.Scores.String()))
	}
	fmt.Fprintf(&b, "reason: %s\ndecision: %d", esc(res.Reason), res.DecisionID)
	if len(failures) > 0 {
		fmt.Fprintf(&b, "\nfailed: %s", esc(strings.Join(failures, ", ")))
	}
	return truncateString(b.String(), 3900, "")
}

// messageLink makes a t.me link to a message of a supergroup
func messageLink(chatID int64, msgID int) string {
	s := strconv.FormatInt(chatID, 10)
	if msgID == 0 || !strings.HasPrefix(s, "-100") {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", s[4:], msgID)
}

// HandleProfileCallback handles the buttons of profile cards: Pb ban, Ph not spam, Pu unban
func (l *TelegramListener) HandleProfileCallback(ctx context.Context, q *tbapi.CallbackQuery) error {
	if l.Profile == nil || q.Message == nil || q.Message.Chat.ID != l.adminChatID || len(q.Data) < 3 {
		return nil
	}
	action, idStr := q.Data[:2], q.Data[2:]
	decisionID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return fmt.Errorf("bad profile callback %q: %w", q.Data, err)
	}
	if _, err = l.TbAPI.Request(tbapi.NewCallback(q.ID, "accepted")); err != nil {
		return fmt.Errorf("failed to answer profile callback: %w", err)
	}

	verdict := profile.VerdictHam
	if action == profileBanBtn {
		verdict = profile.VerdictSpam
	}
	rec, first, err := l.Profile.Resolve(ctx, decisionID, q.From.ID, verdict)
	if err != nil {
		return fmt.Errorf("failed to resolve decision %d: %w", decisionID, err)
	}
	status := "already answered by someone else"
	retried, _ := l.profileRetry.Load(decisionID)
	keepButtons := false
	if first || retried == action { // a failed action of the same button is repeated, the others are already answered
		var actErr error
		switch action {
		case profileBanBtn:
			actErr = l.banAfterReview(rec.UserID)
			status = "banned"
		case profileHamBtn:
			actErr = l.releaseAfterReview(rec.UserID, false)
			status = "marked not spam"
		case profileUnbanBtn:
			actErr = l.releaseAfterReview(rec.UserID, true)
			status = "unbanned"
		default:
			return fmt.Errorf("unknown profile action %q", action)
		}
		if actErr != nil {
			status += fmt.Sprintf(" (failed: %v, press again to retry)", actErr)
			l.profileRetry.Store(decisionID, action)
			keepButtons = true
		} else {
			l.profileRetry.Delete(decisionID)
		}
	}

	edit := tbapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID,
		fmt.Sprintf("%s\n\n%s by %s", q.Message.Text, status, q.From.UserName))
	edit.ReplyMarkup = &tbapi.InlineKeyboardMarkup{InlineKeyboard: [][]tbapi.InlineKeyboardButton{}}
	if keepButtons && q.Message.ReplyMarkup != nil {
		edit.ReplyMarkup = q.Message.ReplyMarkup
	}
	if _, err = l.TbAPI.Send(edit); err != nil {
		return fmt.Errorf("failed to edit profile card: %w", err)
	}
	return nil
}

// banAfterReview bans a restricted user for real in the group and in the linked channel
func (l *TelegramListener) banAfterReview(userID int64) error {
	if err := l.Bot.RemoveApprovedUser(userID); err != nil {
		log.Printf("[DEBUG] profile: user %d was not in the approved list: %v", userID, err)
	}
	userStr := fmt.Sprintf("id:%d", userID)
	err := banUserOrChannel(banRequest{duration: bot.PermanentBanDuration, userID: userID, userName: userStr, chatID: l.chatID,
		dry: l.Dry, training: l.TrainingMode, tbAPI: l.TbAPI, restrict: l.SoftBanMode})
	if l.linkedChannelID != 0 && !l.SoftBanMode {
		if cerr := banUserOrChannel(banRequest{duration: bot.PermanentBanDuration, userID: userID, userName: userStr,
			chatID: l.linkedChannelID, dry: l.Dry, training: l.TrainingMode, tbAPI: l.TbAPI}); cerr != nil {
			log.Printf("[WARN] profile: failed to ban %d in the channel: %v", userID, cerr)
		}
	}
	if l.AggressiveCleanup && !l.Dry && !l.TrainingMode && l.adminHandler != nil {
		go func() {
			if _, derr := l.adminHandler.deleteUserMessages(userID); derr != nil {
				log.Printf("[WARN] profile: aggressive cleanup of %d failed: %v", userID, derr)
			}
		}()
	}
	return err
}

// releaseAfterReview lifts the restriction (and the ban, if banned) and approves the user. Nothing is sent to
// Telegram in dry-run and training mode.
func (l *TelegramListener) releaseAfterReview(userID int64, unban bool) error {
	var firstErr error
	if !l.Dry && !l.TrainingMode {
		if err := l.liftRestrictions(userID); err != nil {
			firstErr = err
		}
		if unban && !l.SoftBanMode {
			for _, chatID := range []int64{l.chatID, l.linkedChannelID} {
				if chatID == 0 {
					continue
				}
				// only_if_banned keeps a member who is not banned in the chat
				_, err := l.TbAPI.Request(tbapi.UnbanChatMemberConfig{
					ChatMemberConfig: tbapi.ChatMemberConfig{UserID: userID, ChatConfig: tbapi.ChatConfig{ChatID: chatID}},
					OnlyIfBanned:     true,
				})
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("unban in %d: %w", chatID, err)
				}
			}
		}
	}
	if err := l.Bot.AddApprovedUser(userID, ""); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("approve: %w", err)
	}
	return firstErr
}

// liftRestrictions gives the user the default permissions of the group
func (l *TelegramListener) liftRestrictions(userID int64) error {
	perms := &tbapi.ChatPermissions{CanSendMessages: true, CanSendAudios: true, CanSendDocuments: true, CanSendPhotos: true,
		CanSendVideos: true, CanSendVideoNotes: true, CanSendVoiceNotes: true, CanSendOtherMessages: true, CanSendPolls: true,
		CanAddWebPagePreviews: true, CanReactToMessages: true, CanInviteUsers: true}
	if info, err := l.TbAPI.GetChat(tbapi.ChatInfoConfig{ChatConfig: tbapi.ChatConfig{ChatID: l.chatID}}); err == nil &&
		info.Permissions != nil {
		perms = info.Permissions
	}
	if _, err := l.TbAPI.Request(tbapi.RestrictChatMemberConfig{
		ChatMemberConfig: tbapi.ChatMemberConfig{ChatConfig: tbapi.ChatConfig{ChatID: l.chatID}, UserID: userID},
		Permissions:      perms,
	}); err != nil {
		return fmt.Errorf("lift restrictions: %w", err)
	}
	return nil
}
