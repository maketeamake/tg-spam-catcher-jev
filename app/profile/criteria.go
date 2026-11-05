package profile

import (
	"bytes"
	_ "embed" // for the built-in criteria
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

//go:embed criteria.yml
var defaultCriteria []byte

// Boolean is a yes/no question with the definitions of both answers
type Boolean struct {
	Instructions string `yaml:"instructions"`
	True         string `yaml:"true"`
	False        string `yaml:"false"`
}

// Criteria holds the wording of every question asked to jev: it is data, not code, so it can be tuned
// without rebuilding. Changing the wording invalidates the thresholds calibrated with it.
type Criteria struct {
	CommentClass struct {
		Instructions string            `yaml:"instructions"`
		Classes      map[string]string `yaml:"classes"`
	} `yaml:"comment_class"`
	CommentRelevance struct {
		Instructions string   `yaml:"instructions"`
		Levels       []string `yaml:"levels"`
	} `yaml:"comment_relevance"`
	Bait             Boolean `yaml:"bait"`
	ProfileAdult     Boolean `yaml:"profile_adult"`
	ProfileDrugs     Boolean `yaml:"profile_drugs"`
	ProfileScam      Boolean `yaml:"profile_scam"`
	PhotoProvocative Boolean `yaml:"photo_provocative"`
}

// LoadCriteria reads criteria from a yaml file, an empty path gives the built-in wording
func LoadCriteria(path string) (*Criteria, error) {
	data := defaultCriteria
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil { //nolint:gosec // path is an operator-provided option
			return nil, fmt.Errorf("can't read criteria file: %w", err)
		}
	}
	var c Criteria
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("can't parse criteria: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Criteria) validate() error {
	if c.CommentClass.Instructions == "" {
		return fmt.Errorf("criteria: comment_class.instructions must not be empty")
	}
	if _, ok := c.CommentClass.Classes[classOK]; !ok || len(c.CommentClass.Classes) < 2 {
		return fmt.Errorf("criteria: comment_class.classes must contain %q and at least one spam class", classOK)
	}
	if c.CommentRelevance.Instructions == "" || len(c.CommentRelevance.Levels) < 2 || len(c.CommentRelevance.Levels) > 10 {
		return fmt.Errorf("criteria: comment_relevance needs instructions and 2 to 10 levels")
	}
	for name, b := range map[string]Boolean{"bait": c.Bait, "profile_adult": c.ProfileAdult, "profile_drugs": c.ProfileDrugs,
		"profile_scam": c.ProfileScam, "photo_provocative": c.PhotoProvocative} {
		if b.Instructions == "" || b.True == "" || b.False == "" {
			return fmt.Errorf("criteria: %s needs instructions, true and false", name)
		}
	}
	return nil
}
