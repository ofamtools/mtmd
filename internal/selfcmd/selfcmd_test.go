package selfcmd

import (
	"os"
	"reflect"
	"testing"
)

func TestZeroValueIsUpstream(t *testing.T) {
	Set(nil)
	want, _ := os.Executable()
	got, err := Exe()
	if err != nil || got != want {
		t.Fatalf("Exe() = %q, %v; want %q", got, err, want)
	}
	if Embedded() || Words() != nil {
		t.Fatalf("zero value is embedded: %v", Words())
	}
	if a := Argv("x", "daemon"); !reflect.DeepEqual(a, []string{"x", "daemon"}) {
		t.Fatalf("Argv = %v", a)
	}
}

func TestPrefix(t *testing.T) {
	t.Cleanup(func() { Set(nil) })
	Set([]string{"/bin/host", "mtmd"})
	if e, _ := Exe(); e != "/bin/host" {
		t.Fatalf("Exe() = %q", e)
	}
	if a := Argv("/proc/self/exe", "daemon", "--x"); !reflect.DeepEqual(a, []string{"/proc/self/exe", "mtmd", "daemon", "--x"}) {
		t.Fatalf("Argv = %v", a)
	}
	Set([]string{"", "mtmd"})
	if e, _ := Exe(); e == "" || e == "/bin/host" {
		t.Fatalf("empty exe must mean os.Executable, got %q", e)
	}
	if !Embedded() {
		t.Fatal("not embedded")
	}
}
