package vision

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type Evidence struct {
	Description     string   `json:"description"`
	VisibleText     string   `json:"visible_text"`
	RelevantDetails []string `json:"relevant_details"`
	Uncertainties   []string `json:"uncertainties"`
}

func parseEvidence(raw string) (Evidence, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		lines := strings.Split(raw, "\n")
		if len(lines) >= 3 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			raw = strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
		}
	}

	type evidenceWire struct {
		Description     *string   `json:"description"`
		VisibleText     *string   `json:"visible_text"`
		RelevantDetails *[]string `json:"relevant_details"`
		Uncertainties   *[]string `json:"uncertainties"`
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var wire evidenceWire
	if err := dec.Decode(&wire); err != nil {
		return Evidence{}, fmt.Errorf("decode evidence JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return Evidence{}, errors.New("decode evidence JSON: trailing JSON value")
		}
		return Evidence{}, fmt.Errorf("decode evidence JSON: %w", err)
	}
	if wire.Description == nil || wire.VisibleText == nil || wire.RelevantDetails == nil || wire.Uncertainties == nil {
		return Evidence{}, errors.New("evidence must contain description, visible_text, relevant_details, and uncertainties")
	}
	if strings.TrimSpace(*wire.Description) == "" {
		return Evidence{}, errors.New("evidence description is required")
	}
	return Evidence{
		Description:     *wire.Description,
		VisibleText:     *wire.VisibleText,
		RelevantDetails: *wire.RelevantDetails,
		Uncertainties:   *wire.Uncertainties,
	}, nil
}
