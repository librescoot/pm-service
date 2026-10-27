package systemd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstalledUnitGatesBatteryOnReadiness(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "..", "librescoot-pm.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Type=notify", "NotifyAccess=main", "Before=librescoot-battery.service", "RequiredBy=librescoot-battery.service"} {
		if !strings.Contains(string(unit), "\n"+line+"\n") {
			t.Fatalf("unit missing %q", line)
		}
	}
	if strings.Contains(string(unit), "After=librescoot-battery.service") {
		t.Fatal("PM waits for the battery it must precede")
	}
	for _, tool := range []string{"systemctl", "systemd-analyze"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s required for offline unit installation test", tool)
		}
	}
	root := t.TempDir()
	units := filepath.Join(root, "usr/lib/systemd/system")
	if err := os.MkdirAll(units, 0755); err != nil {
		t.Fatal(err)
	}
	write := func(path, data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(units, "librescoot-pm.service"), string(unit), 0644)
	for _, name := range []string{"valkey.service", "librescoot-vehicle.service", "librescoot-settings.service", "librescoot-battery.service"} {
		write(filepath.Join(units, name), "[Unit]\nDescription=Test dependency\n[Service]\nExecStart=/usr/bin/pm-service\n", 0644)
	}
	for _, name := range []string{"multi-user.target", "sysinit.target", "basic.target", "shutdown.target"} {
		write(filepath.Join(units, name), "[Unit]\nDescription=Test target\n", 0644)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/bin"), 0755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "usr/bin/pm-service"), "#!/bin/sh\nexit 0\n", 0755)
	cmd := exec.Command("systemctl", "--root="+root, "enable", "librescoot-pm.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("enable: %v\n%s", err, output)
	}
	link := filepath.Join(root, "etc/systemd/system/librescoot-battery.service.requires/librescoot-pm.service")
	if _, err := os.Readlink(link); err != nil {
		t.Fatalf("battery does not require PM after enable: %v", err)
	}
	cmd = exec.Command("systemd-analyze", "--root="+root, "verify", "librescoot-battery.service", "librescoot-pm.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unit graph: %v\n%s", err, output)
	}
}
