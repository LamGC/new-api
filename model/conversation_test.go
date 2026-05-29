package model

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
)

func TestConversationResponse_SetAndGetMessages(t *testing.T) {
	messages := []dto.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	}

	c := &ConversationResponse{}
	err := c.SetMessages(messages)
	if err != nil {
		t.Fatalf("SetMessages error: %v", err)
	}
	if len(c.Messages) == 0 {
		t.Fatal("Messages should not be empty after SetMessages")
	}

	got, err := c.GetMessages()
	if err != nil {
		t.Fatalf("GetMessages error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(got))
	}
	if got[0].Role != "user" || got[0].StringContent() != "Hello" {
		t.Errorf("unexpected first message: role=%s content=%s", got[0].Role, got[0].StringContent())
	}
	if got[1].Role != "assistant" || got[1].StringContent() != "Hi there!" {
		t.Errorf("unexpected second message: role=%s content=%s", got[1].Role, got[1].StringContent())
	}
}

func TestConversationResponse_EmptyMessages(t *testing.T) {
	c := &ConversationResponse{}
	msgs, err := c.GetMessages()
	if err != nil {
		t.Fatalf("GetMessages error: %v", err)
	}
	if msgs != nil {
		t.Errorf("expected nil for empty messages, got %v", msgs)
	}
}
