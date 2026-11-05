package profile

import "strings"

// state is what the questions refer to. Empty parts are omitted.
type state struct {
	Event         string      `json:"event"`
	Post          *textObject `json:"post,omitempty"`
	Comment       *commentObj `json:"comment,omitempty"`
	User          userObject  `json:"user"`
	ProfilePhotos []string    `json:"profile_photos,omitempty"`
}

type textObject struct {
	Text string `json:"text"`
}

type commentObj struct {
	Text        string `json:"text"`
	ReplyToText string `json:"reply_to_text,omitempty"`
}

type userObject struct {
	FirstName       string   `json:"first_name,omitempty"`
	LastName        string   `json:"last_name,omitempty"`
	Username        string   `json:"username,omitempty"`
	IsPremium       bool     `json:"is_premium,omitempty"`
	Bio             string   `json:"bio,omitempty"`
	PersonalChannel *Channel `json:"personal_channel,omitempty"`
}

func (s *Service) state(ev Event, d *Dossier, commentText string, images []Image) state {
	st := state{Event: string(ev.Kind), User: userObject{FirstName: d.User.FirstName, LastName: d.User.LastName,
		Username: d.User.Username, IsPremium: d.User.IsPremium, Bio: d.Bio, PersonalChannel: d.Channel}}
	if ev.PostText != "" {
		st.Post = &textObject{Text: trunc(ev.PostText, maxPostRunes)}
	}
	if commentText != "" {
		st.Comment = &commentObj{Text: trunc(commentText, maxPostRunes), ReplyToText: trunc(ev.ReplyText, maxChannelPostRunes)}
	}
	for _, img := range images {
		st.ProfilePhotos = append(st.ProfilePhotos, img.Ref)
	}
	return st
}

// questions builds the questions that make sense for the event. Comment questions need a comment, the relevance
// question also needs the post, and profile questions need something in the profile.
func (s *Service) questions(ev Event, d *Dossier, hasComment, hasPhotos bool) map[string]Question {
	c := s.cfg.Criteria
	q := map[string]Question{}
	if hasComment {
		q[qClass] = Question{Type: "choice", Instructions: c.CommentClass.Instructions, Criteria: c.CommentClass.Classes}
		q[qBait] = boolQuestion(c.Bait, false)
		if ev.PostText != "" {
			q[qRelevance] = Question{Type: "score", Instructions: c.CommentRelevance.Instructions, Criteria: c.CommentRelevance.Levels}
		}
	}
	if !d.Empty() {
		q[qAdult] = boolQuestion(c.ProfileAdult, hasPhotos)
		q[qDrugs] = boolQuestion(c.ProfileDrugs, hasPhotos)
		q[qScam] = boolQuestion(c.ProfileScam, hasPhotos)
	}
	if hasPhotos {
		q[qPhoto] = boolQuestion(c.PhotoProvocative, true)
	}
	return q
}

// boolQuestion makes a yes/no question; without photos the mention of avatars is dropped from the wording
func boolQuestion(b Boolean, photos bool) Question {
	instr := b.Instructions
	if !photos {
		instr = strings.ReplaceAll(instr, profilePhotosHint, "")
	}
	return Question{Type: "noul", Instructions: instr, Criteria: map[string]string{"true": b.True, "false": b.False}}
}
