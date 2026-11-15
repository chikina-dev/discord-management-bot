package test

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type hostUnit map[string]map[string]string

func readHostUnit(t *testing.T, name string) hostUnit {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "ansible", "files", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	unit := hostUnit{}
	section := ""
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			unit[section] = map[string]string{}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || section == "" || strings.HasSuffix(line, "\\") {
			t.Fatalf("unsupported unit syntax in %s: %s", name, line)
		}
		if previous := unit[section][key]; previous != "" {
			value = previous + " " + value
		}
		unit[section][key] = value
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return unit
}

func blockedByFailedPrerequisite(name string, failed map[string]bool, units map[string]hostUnit, visiting map[string]bool) bool {
	if failed[name] {
		return true
	}
	if visiting[name] {
		return true
	}
	visiting[name] = true
	defer delete(visiting, name)
	for _, directive := range []string{"Requires", "Requisite", "BindsTo"} {
		for _, dependency := range strings.Fields(units[name]["Unit"][directive]) {
			if blockedByFailedPrerequisite(dependency, failed, units, visiting) {
				return true
			}
		}
	}
	return false
}

func TestHostFailureDoesNotSuppressGatewayProcessRetry(t *testing.T) {
	units := map[string]hostUnit{
		"discord-gateway.service":     readHostUnit(t, "discord-gateway.service"),
		"discord-gateway-dns.service": readHostUnit(t, "discord-gateway-dns.service"),
	}
	for _, failed := range []string{"discord-gateway-dns.service", "wg-quick@wg0.service"} {
		if blockedByFailedPrerequisite("discord-gateway.service", map[string]bool{failed: true}, units, map[string]bool{}) {
			t.Fatalf("%s failure prevents the gateway process from starting and using its restart policy", failed)
		}
	}
	gateway := units["discord-gateway.service"]
	if gateway["Service"]["Restart"] != "on-failure" || gateway["Unit"]["StartLimitIntervalSec"] != "0" {
		t.Fatal("gateway bind failures must remain recoverable through systemd process retry")
	}
	dns := units["discord-gateway-dns.service"]
	if dns["Service"]["Restart"] != "on-failure" || dns["Unit"]["StartLimitIntervalSec"] != "0" {
		t.Fatal("temporary DNS/IMDS/AWS failure must be retried by its independent systemd lifecycle")
	}
	if gateway["Service"]["ExecStart"] == dns["Service"]["ExecStart"] {
		t.Fatal("DNS and gateway lifecycles must execute separate operations")
	}
}
