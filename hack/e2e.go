/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// e2e.go runs the e2e test suite. No non-standard package dependencies; call with "go run".
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	isup             = flag.Bool("isup", false, "Check to see if the e2e cluster is up, then exit.")
	build            = flag.Bool("build", false, "If true, build a new release. Otherwise, use whatever is there.")
	up               = flag.Bool("up", false, "If true, start the the e2e cluster. If cluster is already up, recreate it.")
	push             = flag.Bool("push", false, "If true, push to e2e cluster. Has no effect if -up is true.")
	pushup           = flag.Bool("pushup", false, "If true, push to e2e cluster if it's up, otherwise start the e2e cluster.")
	down             = flag.Bool("down", false, "If true, tear down the cluster before exiting.")
	orderseed        = flag.Int64("orderseed", 0, "If non-zero, seed of random test shuffle order. (Otherwise random.)")
	test             = flag.Bool("test", false, "Run all tests in hack/e2e-suite.")
	tests            = flag.String("tests", "", "Run only tests in hack/e2e-suite matching this glob. Ignored if -test is set.")
	times            = flag.Int("times", 1, "Number of times each test is eligible to be run. Individual order is determined by shuffling --times instances of each test using --orderseed (like a multi-deck shoe of cards).")
	root             = flag.String("root", absOrDie(filepath.Clean(filepath.Join(path.Base(os.Args[0]), ".."))), "Root directory of kubernetes repository.")
	tap              = flag.Bool("tap", false, "Enable Test Anything Protocol (TAP) output (disables --verbose, only failure output recorded)")
	verbose          = flag.Bool("v", false, "If true, print all command output.")
	trace_bash       = flag.Bool("trace-bash", false, "If true, pass -x to bash to trace all bash commands")
	checkVersionSkew = flag.Bool("check_version_skew", true, ""+
		"By default, verify that client and server have exact version match. "+
		"You can explicitly set to false if you're, e.g., testing client changes "+
		"for which the server version doesn't make a difference.")

	cfgCmd = flag.String("cfg", "", "If nonempty, pass this as an argument, and call kubecfg. Implies -v.")
	ctlCmd = flag.String("ctl", "", "If nonempty, pass this as an argument, and call kubectl. Implies -v. (-test, -cfg, -ctl are mutually exclusive)")
)

var signals = make(chan os.Signal, 100)

func absOrDie(path string) string {
	out, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return out
}

type TestResult struct {
	Pass int
	Fail int
}

type ResultsByTest map[string]TestResult

func main() {
	flag.Parse()
	signal.Notify(signals, os.Interrupt)

	if *tap {
		fmt.Printf("TAP version 13\n")
		log.SetPrefix("# ")

		// TODO: this limitation is fixable by moving runBash to
		// outputing to temp files, which still lets people check on
		// stuck things interactively. The current stdout/stderr
		// approach isn't really going to work with TAP, though.
		*verbose = false
	}

	if *test {
		*tests = "*"
	}

	if *isup {
		status := 1
		if IsUp() {
			status = 0
			log.Printf("Cluster is UP")
		} else {
			log.Printf("Cluster is DOWN")
		}
		os.Exit(status)
	}

	if *build {
		if !runBash("build-release", `test-build-release`) {
			log.Fatal("Error building. Aborting.")
		}
	}

	if *pushup {
		if IsUp() {
			log.Printf("e2e cluster is up, pushing.")
			*up = false
			*push = true
		} else {
			log.Printf("e2e cluster is down, creating.")
			*up = true
			*push = false
		}
	}
	if *up {
		if !Up() {
			log.Fatal("Error starting e2e cluster. Aborting.")
		}
	} else if *push {
		if !runBash("push", path.Join(*root, "/cluster/kube-push.sh")) {
			log.Fatal("Error pushing e2e cluster. Aborting.")
		}
	}

	failure := false
	switch {
	case *cfgCmd != "":
		failure = !runBash("'kubecfg "+*cfgCmd+"'", "$KUBECFG "+*cfgCmd)
	case *ctlCmd != "":
		failure = !runBash("'kubectl "+*ctlCmd+"'", "$KUBECTL "+*ctlCmd)
	case *tests != "":
		failure = PrintResults(Test())
	}

	if *down {
		TearDown()
	}

	if failure {
		os.Exit(1)
	}
}

func TearDown() {
	runBash("teardown", "test-teardown")
}

func Up() bool {
	if !tryUp() {
		log.Printf("kube-up failed; will tear down and retry. (Possibly your cluster was in some partially created state?)")
		TearDown()
		return tryUp()
	}
	return true
}

// Is the e2e cluster up?
func IsUp() bool {
	return runBash("get status", `$KUBECFG -server_version`)
}

func tryUp() bool {
	return runBash("up", path.Join(*root, "/cluster/kube-up.sh; test-setup;"))
}

// Fisher-Yates shuffle using the given RNG r
func shuffleStrings(strings []string, r *rand.Rand) {
	for i := len(strings) - 1; i > 0; i-- {
		j := r.Intn(i + 1)
		strings[i], strings[j] = strings[j], strings[i]
	}
}

func Test() (results ResultsByTest) {
	defer runBashUntil("watchEvents", "$KUBECTL --watch-only get events")()

	if !IsUp() {
		log.Fatal("Testing requested, but e2e cluster not up!")
	}

	// run tests!
	dir, err := os.Open(filepath.Join(*root, "hack", "e2e-suite"))
	if err != nil {
		log.Fatal("Couldn't open e2e-suite dir")
	}
	defer dir.Close()
	names, err := dir.Readdirnames(0)
	if err != nil {
		log.Fatal("Couldn't read names in e2e-suite dir")
	}

	toRun := make([]string, 0, len(names))
	for i := range names {
		name := names[i]
		if name == "." || name == ".." {
			continue
		}
		if match, err := path.Match(*tests, name); !match && err == nil {
			continue
		}
		if err != nil {
			log.Fatal("Bad test pattern: %v", tests)
		}
		toRun = append(toRun, name)
	}

	if *orderseed == 0 {
		// Use low order bits of NanoTime as the default seed. (Using
		// all the bits makes for a long, very similar looking seed
		// between runs.)
		*orderseed = time.Now().UnixNano() & (1<<32 - 1)
	}
	sort.Strings(toRun)
	if *times != 1 {
		if *times <= 0 {
			log.Fatal("Invalid --times (negative or no testing requested)!")
		}
		newToRun := make([]string, 0, *times*len(toRun))
		for i := 0; i < *times; i++ {
			newToRun = append(newToRun, toRun...)
		}
		toRun = newToRun
	}
	shuffleStrings(toRun, rand.New(rand.NewSource(*orderseed)))
	log.Printf("Running tests matching %v shuffled with seed %#x: %v", *tests, *orderseed, toRun)
	results = ResultsByTest{}
	if *tap {
		fmt.Printf("1..%v\n", len(toRun))
	}
	for i, name := range toRun {
		absName := filepath.Join(*root, "hack", "e2e-suite", name)
		log.Printf("Starting test [%v/%v]: %v", i+1, len(toRun), name)
		testResult := results[name]
		res, stdout, stderr := runBashWithOutputs(name, absName)
		if res {
			fmt.Printf("ok %v - %v\n", i+1, name)
			testResult.Pass++
		} else {
			fmt.Printf("not ok %v - %v\n", i+1, name)
			printBashOutputs("  ", "    ", stdout, stderr)
			testResult.Fail++
		}
		results[name] = testResult
	}

	return
}

func PrintResults(results ResultsByTest) bool {
	failures := 0

	passed := []string{}
	flaky := []string{}
	failed := []string{}
	for test, result := range results {
		if result.Pass > 0 && result.Fail == 0 {
			passed = append(passed, test)
		} else if result.Pass > 0 && result.Fail > 0 {
			flaky = append(flaky, test)
			failures += result.Fail
		} else {
			failed = append(failed, test)
			failures += result.Fail
		}
	}
	sort.Strings(passed)
	sort.Strings(flaky)
	sort.Strings(failed)
	printSubreport("Passed", passed, results)
	printSubreport("Flaky", flaky, results)
	printSubreport("Failed", failed, results)
	if failures > 0 {
		log.Printf("%v test(s) failed.", failures)
	} else {
		log.Printf("Success!")
	}

	return failures > 0
}

func printSubreport(title string, tests []string, results ResultsByTest) {
	report := title + " tests:"

	for _, test := range tests {
		result := results[test]
		report += fmt.Sprintf(" %v[%v/%v]", test, result.Pass, result.Pass+result.Fail)
	}
	log.Printf(report)
}

// All nonsense below is temporary until we have go versions of these things.

func runBashWithOutputs(stepName, bashFragment string) (bool, string, string) {
	cmd := exec.Command("bash", "-s")
	if *trace_bash {
		cmd.Args = append(cmd.Args, "-x")
	}
	cmd.Stdin = strings.NewReader(bashWrap(bashFragment))
	return finishRunning(stepName, cmd)
}

func runBash(stepName, bashFragment string) bool {
	result, _, _ := runBashWithOutputs(stepName, bashFragment)
	return result
}

// call the returned anonymous function to stop.
func runBashUntil(stepName, bashFragment string) func() {
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(bashWrap(bashFragment))
	log.Printf("Running in background: %v", stepName)
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		log.Printf("Unable to start '%v': '%v'", stepName, err)
		return func() {}
	}
	return func() {
		cmd.Process.Signal(os.Interrupt)
		headerprefix := stepName + " "
		lineprefix := "  "
		if *tap {
			headerprefix = "# " + headerprefix
			lineprefix = "# " + lineprefix
		}
		printBashOutputs(headerprefix, lineprefix, string(stdout.Bytes()), string(stderr.Bytes()))
	}
}

func finishRunning(stepName string, cmd *exec.Cmd) (bool, string, string) {
	log.Printf("Running: %v", stepName)
	stdout, stderr := bytes.NewBuffer(nil), bytes.NewBuffer(nil)
	if *verbose {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stdout = stdout
		cmd.Stderr = stderr
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-done:
				return
			case s := <-signals:
				cmd.Process.Signal(s)
			}
		}
	}()

	if err := cmd.Run(); err != nil {
		log.Printf("Error running %v: %v", stepName, err)
		if !*verbose {
			return false, string(stdout.Bytes()), string(stderr.Bytes())
		} else {
			return false, "", ""
		}
	}
	return true, "", ""
}

func printBashOutputs(headerprefix, lineprefix, stdout, stderr string) {
	// The |'s (plus appropriate prefixing) are to make this look
	// "YAMLish" to the Jenkins TAP plugin
	if stdout != "" {
		fmt.Printf("%vstdout: |\n", headerprefix)
		printPrefixedLines(lineprefix, stdout)
	}
	if stderr != "" {
		fmt.Printf("%vstderr: |\n", headerprefix)
		printPrefixedLines(lineprefix, stderr)
	}
}

func printPrefixedLines(prefix, s string) {
	for _, line := range strings.Split(s, "\n") {
		fmt.Printf("%v%v\n", prefix, line)
	}
}

// returns either "", or a list of args intended for appending with the
// kubecfg or kubectl commands (begining with a space).
func kubecfgArgs() string {
	if *checkVersionSkew {
		return " -expect_version_match"
	}
	return ""
}

// returns either "", or a list of args intended for appending with the
// kubectl command (begining with a space).
func kubectlArgs() string {
	if *checkVersionSkew {
		return " --match-server-version"
	}
	return ""
}

func bashWrap(cmd string) string {
	return `
set -o errexit
set -o nounset
set -o pipefail

export KUBE_CONFIG_FILE="config-test.sh"

# TODO(jbeda): This will break on usage if there is a space in
# ${KUBE_ROOT}.  Covert to an array?  Or an exported function?
export KUBECFG="` + *root + `/cluster/kubecfg.sh` + kubecfgArgs() + `"
export KUBECTL="` + *root + `/cluster/kubectl.sh` + kubectlArgs() + `"

source "` + *root + `/cluster/kube-env.sh"
source "` + *root + `/cluster/${KUBERNETES_PROVIDER}/util.sh"

prepare-e2e

` + cmd + `
`
}
