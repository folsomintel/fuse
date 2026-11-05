package hitl

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ComputerPayload is a held computer action. Input is the action body as
// the caller sent it: the action schema belongs to fused, so fuse replays
// it verbatim and only ever rewrites the text of a type.
type ComputerPayload struct {
	Action string          `json:"action"`
	Text   string          `json:"text,omitempty"`
	Input  json.RawMessage `json:"input"`
}

// ComputerResult is the guest's answer to an approved computer action.
type ComputerResult struct {
	Output     string `json:"output,omitempty"`
	Screenshot string `json:"screenshot,omitempty"`
}

// maxEditedText matches fused's cap on a type action's text.
const maxEditedText = 64 << 10

// ParseComputerInput reads the fields a gate matches on out of an action
// body.
func ParseComputerInput(body []byte) (ComputerPayload, error) {
	var a struct {
		Action string `json:"action"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return ComputerPayload{}, fmt.Errorf("malformed action: %w", err)
	}
	if a.Action == "" {
		return ComputerPayload{}, errors.New("action is required")
	}
	return ComputerPayload{Action: a.Action, Text: a.Text, Input: json.RawMessage(body)}, nil
}

// ComputerInput returns the action body an approved computer action runs:
// the original, with the text replaced when the human edited it.
func (in Intervention) ComputerInput() ([]byte, error) {
	if in.Computer == nil {
		return nil, nil
	}
	if in.Decision == nil || in.Decision.EditedText == nil {
		return in.Computer.Input, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(in.Computer.Input, &fields); err != nil {
		return nil, fmt.Errorf("held computer action is not an object: %w", err)
	}
	text, err := json.Marshal(*in.Decision.EditedText)
	if err != nil {
		return nil, err
	}
	fields["text"] = text
	return json.Marshal(fields)
}

// ValidateEditedText checks a human's replacement text for a held type.
func ValidateEditedText(in Intervention, text string) error {
	if in.Kind != KindComputer || in.Computer == nil {
		return errors.New("edited_text only applies to a computer action")
	}
	if in.Computer.Action != "type" {
		return fmt.Errorf("edited_text only applies to a type action, not %s", in.Computer.Action)
	}
	if text == "" {
		return errors.New("edited_text must not be empty; reject the action instead")
	}
	if len(text) > maxEditedText {
		return fmt.Errorf("edited_text must be at most %d bytes", maxEditedText)
	}
	return nil
}
