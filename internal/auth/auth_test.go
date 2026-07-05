package auth

import (
	"testing"
	"time"
)

func TestPasswordHashAndCheck(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !CheckPassword(hash, "correct horse") {
		t.Fatal("correct password should verify")
	}
	if CheckPassword(hash, "wrong") {
		t.Fatal("wrong password must not verify")
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s := NewSigner("platform-secret")
	tok := s.Issue("user-123", time.Hour)
	uid, err := s.Verify(tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if uid != "user-123" {
		t.Fatalf("uid = %q, want user-123", uid)
	}
}

func TestTokenTamperRejected(t *testing.T) {
	s := NewSigner("platform-secret")
	tok := s.Issue("user-123", time.Hour)
	// Flip a byte in the payload; the signature no longer matches.
	tampered := "X" + tok[1:]
	if _, err := s.Verify(tampered); err == nil {
		t.Fatal("a tampered token must not verify")
	}
	// A token signed with a different key must not verify.
	other := NewSigner("different-secret")
	if _, err := s.Verify(other.Issue("user-123", time.Hour)); err == nil {
		t.Fatal("a token signed with another key must not verify")
	}
}

func TestTokenExpired(t *testing.T) {
	s := NewSigner("platform-secret")
	tok := s.Issue("user-123", -time.Minute) // already expired
	if _, err := s.Verify(tok); err == nil {
		t.Fatal("an expired token must not verify")
	}
}
