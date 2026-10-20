package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"time"
)

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func main() {
	data, err := os.ReadFile("/app/dependencies/message.txt")
	if err != nil {
		fail(err)
	}
	_, hidden := os.ReadFile("/etc/runtime-sentinel")
	_, cache := os.ReadDir("/cache")
	write := os.WriteFile("/app/dependencies/forbidden", []byte("must not write"), 0644)
	if hidden == nil || cache == nil || write == nil || os.Getenv("HOST_ONLY") != "" || os.Getenv("VISIBLE") != "explicit" {
		fail(fmt.Errorf("guest capability boundary violated"))
	}
	report := map[string]any{
		"message": "Hello from Wasm", "dependency": string(data),
		"visible": os.Getenv("VISIBLE"), "hostOnly": os.Getenv("HOST_ONLY"),
		"runtimeFileHidden": hidden != nil, "dependencyWriteDenied": write != nil,
		"engineCacheHidden": cache != nil,
		"arguments":         os.Args[1:], "pathInfo": os.Getenv("PATH_INFO"),
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fail(err)
	}
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "probe":
		return
	case "exit37":
		os.Exit(37)
	case "hold", "release":
		if len(os.Args) != 3 || !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(os.Args[2]) {
			fail(fmt.Errorf("explicit safe workload marker required"))
		}
		marker := "/work/" + os.Args[2]
		if os.Args[1] == "release" {
			if err := os.WriteFile(marker, []byte("release"), 0644); err != nil {
				fail(err)
			}
			return
		}
		if err := os.WriteFile(marker+"-started", []byte("guest running"), 0644); err != nil {
			fail(err)
		}
		for {
			if _, err := os.Stat(marker); err == nil {
				return
			} else if !os.IsNotExist(err) {
				fail(err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	default:
		fail(fmt.Errorf("unknown command mode %q", os.Args[1]))
	}
}
