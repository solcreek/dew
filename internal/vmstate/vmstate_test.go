package vmstate

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		PID:       os.Getpid(),
		Phase:     PhaseBooting,
		Mode:      "run",
		Profile:   "minimal",
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok := Read(dir)
	if !ok {
		t.Fatal("Read found nothing after Write")
	}
	if got.PID != want.PID || got.Phase != want.Phase || got.Mode != want.Mode ||
		got.Profile != want.Profile || !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReadMissingFile(t *testing.T) {
	if _, ok := Read(t.TempDir()); ok {
		t.Error("Read reported a state in an empty dir")
	}
}

func TestReadCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(Path(dir), []byte("{not json"), 0600)
	if _, ok := Read(dir); ok {
		t.Error("Read accepted corrupt JSON")
	}
}

// Clear must be ownership-checked: a process exiting late must not
// erase the state a newer process has since published.
func TestClear_OnlyOwner(t *testing.T) {
	dir := t.TempDir()
	Write(dir, State{PID: 1111, Phase: PhaseRunning, Mode: "start"})

	Clear(dir, 2222) // not the owner
	if _, ok := Read(dir); !ok {
		t.Fatal("Clear by non-owner removed the file")
	}

	Clear(dir, 1111)
	if _, ok := Read(dir); ok {
		t.Error("Clear by owner left the file behind")
	}
}

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("our own pid reported dead")
	}
	if Alive(0) || Alive(-1) {
		t.Error("non-positive pid reported alive")
	}

	// A process that has exited and been reaped is dead.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if Alive(cmd.Process.Pid) {
		t.Errorf("reaped pid %d reported alive", cmd.Process.Pid)
	}
}
