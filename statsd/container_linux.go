//go:build linux
// +build linux

package statsd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
)

const (
	// cgroupPath is the path to the cgroup file where we can find the container id if one exists.
	cgroupPath = "/proc/self/cgroup"

	// selfMountinfo is the path to the mountinfo path where we can find the container id in case cgroup namespace is preventing the use of /proc/self/cgroup
	selfMountInfoPath = "/proc/self/mountinfo"

	// mountsPath is the path to the file listing all the mount points
	mountsPath = "/proc/mounts"

	uuidSource      = "[0-9a-f]{8}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{4}[-_][0-9a-f]{12}"
	containerSource = "[0-9a-f]{64}"
	taskSource      = "[0-9a-f]{32}-\\d+"

	containerdSandboxPrefix = "sandboxes"
	containerRegexpStr      = "([0-9a-f]{64})|([0-9a-f]{8}(-[0-9a-f]{4}){4}$)"
	cIDRegexpStr            = `([^\s/]+)/(` + containerRegexpStr + `)/[\S]*hostname`

	// From https://github.com/torvalds/linux/blob/5859a2b1991101d6b978f3feb5325dad39421f29/include/linux/proc_ns.h#L41-L49
	// Currently, host namespace inode number are hardcoded, which can be used to detect
	// if we're running in host namespace or not (does not work when running in DinD)
	hostCgroupNamespaceInode = 0xEFFFFFFB
)

var (
	// expLine matches a line in the /proc/self/cgroup file. It has a submatch for the last element (path), which contains the container ID.
	expLine = regexp.MustCompile(`^\d+:[^:]*:(.+)$`)

	// expContainerID matches contained IDs and sources. Source: https://github.com/Qard/container-info/blob/master/index.js
	expContainerID = regexp.MustCompile(fmt.Sprintf(`(%s|%s|%s)(?:.scope)?$`, uuidSource, containerSource, taskSource))

	cIDMountInfoRegexp = regexp.MustCompile(cIDRegexpStr)

	// initContainerID initializes the container ID.
	initContainerID = internalInitContainerID
)

// parseContainerID finds the first container ID reading from r and returns it.
func parseContainerID(r io.Reader) string {
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		path := expLine.FindStringSubmatch(scn.Text())
		if len(path) != 2 {
			// invalid entry, continue
			continue
		}
		if parts := expContainerID.FindStringSubmatch(path[1]); len(parts) == 2 {
			return parts[1]
		}
	}
	return ""
}

// readContainerID attempts to return the container ID from the provided file path or empty on failure.
func readContainerID(fpath string) string {
	f, err := os.Open(fpath)
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseContainerID(f)
}

// Parsing /proc/self/mountinfo is not always reliable in Kubernetes+containerd (at least)
// We're still trying to use it as it may help in some cgroupv2 configurations (Docker, ECS, raw containerd)
func parseMountinfo(r io.Reader) string {
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		line := scn.Text()
		allMatches := cIDMountInfoRegexp.FindAllStringSubmatch(line, -1)
		if len(allMatches) == 0 {
			continue
		}

		// We're interest in rightmost match
		matches := allMatches[len(allMatches)-1]
		if len(matches) > 0 && matches[1] != containerdSandboxPrefix {
			return matches[2]
		}
	}

	return ""
}

func readMountinfo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	return parseMountinfo(f)
}

// isCgroupV1 checks if Cgroup V1 is used
func isCgroupV1(mountsPath string) bool {
	f, err := os.Open(mountsPath)
	if err != nil {
		return false
	}
	defer f.Close()

	scn := bufio.NewScanner(f)
	for scn.Scan() {
		line := scn.Text()

		tokens := strings.Fields(line)
		if len(tokens) >= 3 {
			fsType := tokens[2]
			if fsType == "cgroup" {
				return true
			}
		}
	}

	return false
}

// parseCgroupV2MountPath parses the cgroup mount path from /proc/mounts
// It returns an empty string if cgroup v2 is not used
func parseCgroupV2MountPath(r io.Reader) string {
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		line := scn.Text()
		// a correct line line should be formatted as `cgroup2 <path> cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0`
		tokens := strings.Fields(line)
		if len(tokens) >= 3 {
			fsType := tokens[2]
			if fsType == "cgroup2" {
				return tokens[1]
			}
		}
	}
	return ""
}

// parseCgroupV2NodePath parses the cgroup node path from /proc/self/cgroup
// It returns an empty string if cgroup v2 is not used
// With respect to https://man7.org/linux/man-pages/man7/cgroups.7.html#top_of_page, in cgroupv2, only 0::<path> should exist
func parseCgroupV2NodePath(r io.Reader) string {
	scn := bufio.NewScanner(r)
	for scn.Scan() {
		line := scn.Text()
		// The cgroup node path is the last element of the line starting with "0::"
		if strings.HasPrefix(line, "0::") {
			return line[3:]
		}
	}
	return ""
}

// getCgroupV2Inode returns the cgroup v2 node inode if it exists otherwise an empty string.
// The inode is prefixed by "in-" and is used by the agent to retrieve the container ID.
func getCgroupV2Inode(mountsPath, cgroupPath string) string {
	// Retrieve a cgroup mount point from /proc/mounts
	f, err := os.Open(mountsPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	cgroupMountPath := parseCgroupV2MountPath(f)

	if cgroupMountPath == "" {
		return ""
	}

	// Parse /proc/self/cgroup to retrieve the cgroup node path
	f, err = os.Open(cgroupPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	cgroupNodePath := parseCgroupV2NodePath(f)
	if cgroupNodePath == "" {
		return ""
	}

	// Retrieve the cgroup inode from the cgroup mount and cgroup node path
	fi, err := os.Stat(path.Clean(cgroupMountPath + cgroupNodePath))
	if err != nil {
		return ""
	}

	return fmt.Sprintf("in-%d", fi.Sys().(*syscall.Stat_t).Ino)
}

func isHostCgroupNamespace() bool {
	fi, err := os.Stat("/proc/self/ns/cgroup")
	if err != nil {
		return false
	}

	inode := fi.Sys().(*syscall.Stat_t).Ino

	return inode == hostCgroupNamespaceInode
}

// internalInitContainerID initializes the container ID.
// It can either be provided by the user or read from cgroups.
func internalInitContainerID(userProvidedID string, cgroupFallback bool) {
	initOnce.Do(func() {
		if userProvidedID != "" {
			containerID = userProvidedID
			return
		}

		if cgroupFallback {
			if isCgroupV1(mountsPath) || isHostCgroupNamespace() {
				containerID = readContainerID(cgroupPath)
			} else {
				containerID = readMountinfo(selfMountInfoPath)
			}
			if containerID != "" {
				containerID = getCgroupV2Inode(mountsPath, cgroupPath)
			}
		}
	})
}
