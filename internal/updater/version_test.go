package updater

import "testing"

func TestNewerVersion(t *testing.T) {
	newer, err := newerVersion("1.2.3", "v1.3.0")
	if err != nil || !newer {
		t.Fatalf("expected newer version: newer=%t err=%v", newer, err)
	}
	newer, err = newerVersion("1.3.0", "v1.3.0")
	if err != nil || newer {
		t.Fatalf("equal version must not update: newer=%t err=%v", newer, err)
	}
}

func TestVersionsEqual(t *testing.T) {
	if !versionsEqual("1.2.3", "v1.2.3") || versionsEqual("1.2.4", "v1.2.3") {
		t.Fatal("unexpected semantic version equality result")
	}
}
