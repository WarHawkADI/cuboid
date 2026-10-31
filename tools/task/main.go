// Command task is the project's task runner, so Windows laptops work without
// make. The Makefile forwards every target here.
//
// CUBOID_NATIVE=1 runs against tools/devdb.sh and tools/stack.sh instead of
// Docker Compose: the bench tool then stops and restarts Cuboid with them.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

var native = os.Getenv("CUBOID_NATIVE") == "1"

func compose(args ...string) []string { return append([]string{"docker", "compose"}, args...) }

func bench(args ...string) [][]string {
	return [][]string{append([]string{"go", "run", "./cmd/bench"}, args...)}
}

// verify is the quick gate: no running stack needed.
func verify() [][]string {
	return [][]string{{"go", "vet", "./..."}, {"go", "test", "./..."}, {"go", "run", "./tools/contrast"}, {"npm", "--prefix", "web", "run", "build"}}
}

var tasks = map[string]func() [][]string{
	"setup": func() [][]string { return [][]string{{"go", "mod", "download"}, {"npm", "--prefix", "web", "ci"}} },
	"up":    func() [][]string { return [][]string{compose("up", "-d", "--build")} },
	"down":  func() [][]string { return [][]string{compose("down")} },
	"seed": func() [][]string {
		if native {
			return [][]string{{"go", "run", "./cmd/seed", "--tier", "small"}}
		}
		return [][]string{compose("--profile", "seed", "run", "--rm", "seed")}
	},
	"seed-main": func() [][]string {
		if native {
			return [][]string{{"go", "run", "./cmd/seed", "--tier", "main"}}
		}
		return [][]string{compose("--profile", "seed", "run", "--rm", "seed", "seed", "--tier", "main")}
	},
	"demo": func() [][]string {
		return [][]string{compose("up", "-d", "--build"), compose("--profile", "seed", "run", "--rm", "seed"),
			{"go", "run", "./tools/task", "demo-url"}}
	},
	"demo-url": func() [][]string { return [][]string{{"go", "version"}} },
	"keys": func() [][]string {
		if native {
			return [][]string{{"go", "run", "./cmd/cuboid", "keys"}}
		}
		return [][]string{compose("exec", "cuboid", "cuboid", "keys")}
	},
	"test":   func() [][]string { return [][]string{{"go", "test", "./..."}} },
	"verify": verify,
	"verify-full": func() [][]string {
		return append(verify(),
			[]string{"go", "test", "-tags", "e2e", "./tests/..."},
			[]string{"go", "test", "./internal/reference/", "-rapid.checks=100000"},
			[]string{"go", "run", "golang.org/x/vuln/cmd/govulncheck@latest", "./..."},
			[]string{"npm", "--prefix", "web", "audit", "--omit=dev"},
			[]string{"npm", "--prefix", "web", "exec", "--", "node", "e2e/shots.mjs"},
		)
	},
	"clean-clone-check":        func() [][]string { return [][]string{{"bash", "tools/clean-clone.sh", "native"}} },
	"docker-clean-clone-check": func() [][]string { return [][]string{{"bash", "tools/clean-clone.sh", "docker"}} },
	"demo-check":               func() [][]string { return bench("demo-check") },
	"speed-test":               func() [][]string { return bench("speed") },
	"freshness-test":           func() [][]string { return bench("freshness") },
	"safety-test":              func() [][]string { return bench("safety") },
	"correctness-test":         func() [][]string { return bench("correctness") },
	"growth-test":              func() [][]string { return bench("growth") },
	"targets":                  func() [][]string { return bench("targets") },
	"scorecard":                func() [][]string { return bench("scorecard") },
	"report":                   func() [][]string { return bench("report") },
	"bench":                    func() [][]string { return append(bench("steps"), bench("report")...) },
	"chaos":                    func() [][]string { return bench("chaos") },
	"drill":                    func() [][]string { return bench("demo-check") },
	"redteam":                  func() [][]string { return bench("redteam") },
	"isolation":                func() [][]string { return bench("isolation", "-requests", "100000") },
	"isolation-full":           func() [][]string { return bench("isolation", "-requests", "1000000") },
	"rebuild":                  func() [][]string { return bench("rebuild") },
	"web":                      func() [][]string { return [][]string{{"npm", "--prefix", "web", "run", "build"}} },
	"contrast":                 func() [][]string { return [][]string{{"go", "run", "./tools/contrast"}} },
	"disclosure":               func() [][]string { return [][]string{{"go", "run", "./tools/disclosure"}} },
	"critique":                 func() [][]string { return [][]string{{"go", "run", "./tools/screenshot-critique"}} },
}

func main() {
	if len(os.Args) != 2 || tasks[os.Args[1]] == nil {
		var names []string
		for k := range tasks {
			names = append(names, k)
		}
		sort.Strings(names)
		fmt.Fprintln(os.Stderr, "usage: go run ./tools/task <target>\ntargets:", strings.Join(names, " "))
		os.Exit(2)
	}
	if native && runtime.GOOS == "windows" {
		os.Setenv("BENCH_CUBOID_STOP", `bash tools/stack.sh down-cuboid`)
		os.Setenv("BENCH_CUBOID_START", `bash tools/stack.sh up-cuboid`)
	}
	if os.Args[1] == "demo-url" {
		fmt.Println("Open http://localhost:8080/monitor")
		return
	}
	for _, c := range tasks[os.Args[1]]() {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "task %s: %s: %v\n", os.Args[1], strings.Join(c, " "), err)
			os.Exit(1)
		}
	}
}
