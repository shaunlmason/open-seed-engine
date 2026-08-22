// Package card parses and serializes task cards: markdown files with YAML
// frontmatter, living at tasks/<id>.md on the seed-state ref. The body is the
// work order — untrusted input (R3); bookkeeping blocks are written only by
// the shim as verb side effects (card.schema.json).
package card

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Claim struct {
	Actor        string `yaml:"actor"`
	Token        string `yaml:"token"`
	ClaimedAt    string `yaml:"claimed_at"`
	LeaseExpires string `yaml:"lease_expires"`
}

type Review struct {
	Reviewer   string `yaml:"reviewer,omitempty"`
	ReviewedAt string `yaml:"reviewed_at,omitempty"`
	Outcome    string `yaml:"outcome,omitempty"`
	Evidence   string `yaml:"evidence,omitempty"`
}

type Links struct {
	Blocks    []string `yaml:"blocks,omitempty"`
	WaitsFor  []string `yaml:"waits_for,omitempty"`
	RelatesTo []string `yaml:"relates_to,omitempty"`
}

type Card struct {
	ID              string   `yaml:"id"`
	Title           string   `yaml:"title"`
	State           string   `yaml:"state"`
	Priority        string   `yaml:"priority,omitempty"`
	Squad           string   `yaml:"squad,omitempty"`
	Parent          string   `yaml:"parent,omitempty"`
	Labels          []string `yaml:"labels,omitempty"`
	Links           *Links   `yaml:"links,omitempty"`
	BlockedOn       []string `yaml:"blocked_on,omitempty"`
	Author          string   `yaml:"author,omitempty"`
	RejectedAuthors []string `yaml:"rejected_authors,omitempty"`
	Claim           *Claim   `yaml:"claim,omitempty"`
	Review          *Review  `yaml:"review,omitempty"`
	PlanHash        string   `yaml:"plan_hash,omitempty"`
	CreatedAt       string   `yaml:"created_at"`
	UpdatedAt       string   `yaml:"updated_at,omitempty"`

	Body string `yaml:"-"`
}

// NewID mints a hash-based id (never sequential — D1).
func NewID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return "os-" + hex.EncodeToString(b)
}

// NewToken mints a claim fence token.
func NewToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "c-" + hex.EncodeToString(b)
}

func Parse(content string) (*Card, error) {
	rest, ok := strings.CutPrefix(content, "---\n")
	if !ok {
		return nil, fmt.Errorf("card missing frontmatter")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return nil, fmt.Errorf("card frontmatter not terminated")
	}
	var c Card
	if err := yaml.Unmarshal([]byte(front), &c); err != nil {
		return nil, fmt.Errorf("card frontmatter: %w", err)
	}
	c.Body = strings.TrimPrefix(body, "\n")
	if c.ID == "" || c.State == "" {
		return nil, fmt.Errorf("card missing id or state")
	}
	return &c, nil
}

func (c *Card) Serialize() (string, error) {
	front, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	body := c.Body
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return "---\n" + string(front) + "---\n\n" + body, nil
}

// Path is the card's location on the state ref.
func Path(id string) string { return "tasks/" + id + ".md" }
