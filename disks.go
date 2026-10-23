package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

type disk struct {
	Path   string
	Size   int64
	Model  string
	Mounts []string
}

type lsblkDevice struct {
	Name        string          `json:"name"`
	Parent      string          `json:"pkname"`
	Path        string          `json:"path"`
	Size        json.RawMessage `json:"size"`
	Model       string          `json:"model"`
	Tran        string          `json:"tran"`
	RM          json.RawMessage `json:"rm"`
	Type        string          `json:"type"`
	Mountpoints []string        `json:"mountpoints"`
}

func listUSBDisks() ([]disk, error) {
	// --list with PKNAME rather than the default tree: lsblk falls back to a flat
	// list when it can't build the tree (e.g. in containers), which would hide
	// mounted partitions.
	out, err := exec.Command("lsblk", "--json", "--list", "--bytes",
		"--output", "NAME,PKNAME,PATH,SIZE,MODEL,TRAN,RM,TYPE,MOUNTPOINTS").Output()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var parsed struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("lsblk output: %w", err)
	}

	mounts := mountsByDisk(parsed.BlockDevices)
	var disks []disk
	for _, d := range parsed.BlockDevices {
		if d.Type != "disk" || !(d.Tran == "usb" || rawBool(d.RM)) {
			continue
		}
		size, err := rawInt(d.Size)
		// Size 0 is a card reader with no card in it.
		if err != nil || size == 0 {
			continue
		}
		disks = append(disks, disk{
			Path:   d.Path,
			Size:   size,
			Model:  strings.TrimSpace(d.Model),
			Mounts: mounts[d.Name],
		})
	}
	return disks, nil
}

// mountsByDisk maps each top-level device name to the mount points of itself
// and everything below it (partitions, and LVM or LUKS volumes on them).
func mountsByDisk(devices []lsblkDevice) map[string][]string {
	parents := make(map[string]string)
	for _, d := range devices {
		parents[d.Name] = d.Parent
	}
	mounts := make(map[string][]string)
	for _, d := range devices {
		root := d.Name
		for seen := 0; parents[root] != "" && seen < len(devices); seen++ {
			root = parents[root]
		}
		for _, m := range d.Mountpoints {
			if m != "" {
				mounts[root] = append(mounts[root], m)
			}
		}
	}
	return mounts
}

// Older lsblk versions emit RM and SIZE as strings ("0"/"1", "1024").
func rawBool(raw json.RawMessage) bool {
	s := strings.Trim(string(raw), `"`)
	return s == "true" || s == "1"
}

func rawInt(raw json.RawMessage) (int64, error) {
	s := strings.Trim(string(raw), `"`)
	return strconv.ParseInt(s, 10, 64)
}

func expandUser(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	return homeDir() + path[1:]
}

func homeDir() string {
	// Under sudo, HOME is root's, but "~" should mean the invoking user's home.
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := user.Lookup(name); err == nil {
			return u.HomeDir
		}
	}
	home, _ := os.UserHomeDir()
	return home
}
