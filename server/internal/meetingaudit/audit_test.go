package meetingaudit

import "testing"

func TestAuditIdentifiersAreRedactedDeterministically(t *testing.T) {
	first := redact("user-123")
	if first == "" || first == "user-123" || first != redact("user-123") {
		t.Fatalf("redacted identifier = %q", first)
	}
	if emptyAs("", "accepted") != "accepted" || emptyAs("rejected", "accepted") != "rejected" {
		t.Fatal("audit outcome fallback failed")
	}
}
