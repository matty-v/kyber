// Command kyber-disk-capacity reports filesystem capacity from a mounted
// Agent persist volume. It does not walk or modify the volume. The controller
// runs it in a short-lived pod to complete a stopped Agent's CSI filesystem
// resize and reads its bounded JSON from the container termination message.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type measurement struct {
	TotalBytes     uint64 `json:"totalBytes"`
	AvailableBytes uint64 `json:"availableBytes"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: kyber-disk-capacity MOUNT")
		os.Exit(2)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(os.Args[1], &stat); err != nil {
		fmt.Fprintln(os.Stderr, "statfs failed:", err)
		os.Exit(1)
	}
	result := measurement{TotalBytes: stat.Blocks * uint64(stat.Bsize), AvailableBytes: stat.Bavail * uint64(stat.Bsize)}
	encoded, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encoding capacity failed:", err)
		os.Exit(1)
	}
	if err := os.WriteFile("/dev/termination-log", encoded, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "writing capacity report failed:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}
