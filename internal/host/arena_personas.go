package host

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Persona is the audience one judge reads as. Judges with personas judge
// fit to that reader, not overall quality; judges without one read as a
// senior engineer who has to live with the outcome. The yaml shape is the
// one examples/arena/personas.yaml documents.
type Persona struct {
	ID           string   `json:"id" yaml:"id"`
	Role         string   `json:"role" yaml:"role"`
	Briefing     string   `json:"briefing,omitempty" yaml:"briefing"`
	OptimizesFor []string `json:"optimizes_for" yaml:"optimizes_for"`
	// ReadingBudgetWords is how much of each entry this reader reads; the
	// host cuts every entry at it before the judge sees it. 0 reads all.
	ReadingBudgetWords int      `json:"reading_budget_words,omitempty" yaml:"reading_budget_words"`
	SaysNoWhen         []string `json:"says_no_when,omitempty" yaml:"says_no_when"`
	TieBreak           string   `json:"tie_break,omitempty" yaml:"tie_break"`
}

// Name is how arena.md and the API name the persona.
func (p Persona) Name() string {
	if r := strings.TrimSpace(p.Role); r != "" {
		return r
	}
	return strings.TrimSpace(p.ID)
}

func (p Persona) check() error {
	if p.Name() == "" {
		return errors.New("needs an id or a role")
	}
	if len(p.OptimizesFor) == 0 && strings.TrimSpace(p.Briefing) == "" {
		return fmt.Errorf("%s needs a briefing or optimizes_for", p.Name())
	}
	if p.ReadingBudgetWords < 0 {
		return fmt.Errorf("%s has a negative reading budget", p.Name())
	}
	return nil
}

// ParsePersonas reads a personas file: either a list of personas or a
// mapping with personas and an optional shared rubric.
func ParsePersonas(b []byte) ([]Persona, []string, error) {
	var file struct {
		Personas []Persona `yaml:"personas"`
		Rubric   []string  `yaml:"rubric"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		var list []Persona
		if lerr := yaml.Unmarshal(b, &list); lerr != nil {
			return nil, nil, err
		}
		file.Personas = list
	}
	if len(file.Personas) == 0 {
		return nil, nil, errors.New("no personas in the file")
	}
	for i, p := range file.Personas {
		if err := p.check(); err != nil {
			return nil, nil, fmt.Errorf("persona %d: %w", i+1, err)
		}
	}
	return file.Personas, file.Rubric, nil
}

// ParsePriors reads a priors file: either a list of {name, text} or a
// mapping with priors.
func ParsePriors(b []byte) ([]Prior, error) {
	var file struct {
		Priors []Prior `yaml:"priors"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		var list []Prior
		if lerr := yaml.Unmarshal(b, &list); lerr != nil {
			return nil, err
		}
		file.Priors = list
	}
	if len(file.Priors) == 0 {
		return nil, errors.New("no priors in the file")
	}
	return file.Priors, nil
}

// cutWords keeps the first n words of text, layout and all, and says where
// it cut. n <= 0 keeps everything.
func cutWords(text string, n int, note string) string {
	if n <= 0 {
		return text
	}
	words, inWord := 0, false
	for i, r := range text {
		if unicode.IsSpace(r) {
			inWord = false
			continue
		}
		if !inWord {
			inWord = true
			if words++; words > n {
				return strings.TrimRightFunc(text[:i], unicode.IsSpace) + "\n\n[" + note + "]"
			}
		}
	}
	return text
}
