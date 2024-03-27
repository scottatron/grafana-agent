package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	agentBinaryPath = "../../../../../build/grafana-agent-flow"
)

type TestLog struct {
	TestDir    string
	AgentLog   string
	TestOutput string
}

var logChan chan TestLog

func executeCommand(command string, args []string, taskDescription string) {
	fmt.Printf("%s...\n", taskDescription)
	cmd := exec.Command(command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("Error: %s\n", stderr.String())
	}
}

func buildAgent() {
	executeCommand("make", []string{"-C", "../../..", "agent-flow"}, "Building agent")
}

func setupEnvironment() {
	executeCommand("docker-compose", []string{"up", "-d"}, "Setting up environment with Docker Compose")
	// sleep for 5 seconds to ensure that the full env is ready
	time.Sleep(5 * time.Second)
}

func runSingleTest(testDir string, port int) {
	info, err := os.Stat(testDir)
	if err != nil {
		panic(err)
	}
	if !info.IsDir() {
		return
	}

	dirName := filepath.Base(testDir)

	var agentLogBuffer bytes.Buffer
	cmd := exec.Command(agentBinaryPath, "run", "config.river", "--server.http.listen-addr", fmt.Sprintf("0.0.0.0:%d", port))
	cmd.Dir = testDir
	cmd.Stdout = &agentLogBuffer
	cmd.Stderr = &agentLogBuffer

	if err := cmd.Start(); err != nil {
		logChan <- TestLog{
			TestDir:  dirName,
			AgentLog: fmt.Sprintf("Failed to start agent: %v", err),
		}
		return
	}

	testCmd := exec.Command("go", "test")
	testCmd.Dir = testDir
	testOutput, errTest := testCmd.CombinedOutput()

	err = cmd.Process.Kill()
	if err != nil {
		panic(err)
	}

	agentLog := agentLogBuffer.String()

	if errTest != nil {
		logChan <- TestLog{
			TestDir:    dirName,
			AgentLog:   agentLog,
			TestOutput: string(testOutput),
		}
	}

	err = os.RemoveAll(filepath.Join(testDir, "data-agent"))
	if err != nil {
		panic(err)
	}
}

func runAllTests() {
	testDirs, err := filepath.Glob("./tests/*")
	if err != nil {
		panic(err)
	}
	var wg sync.WaitGroup
	port := 12345
	for i, testDir := range testDirs {
		fmt.Println("Running", testDir)
		wg.Add(1)
		go func(td string, offset int) {
			defer wg.Done()
			runSingleTest(td, port+offset)
		}(testDir, i)
	}
	wg.Wait()
}

func cleanUpEnvironment() {
	// TMP tests
	cmd := exec.Command("docker", "ps", "--format", "{{.Names}}")

	// Capture the command's output.
	output, err := cmd.Output()
	if err != nil {
		// If an error occurs, print it to the standard error and exit.
		fmt.Fprintf(os.Stderr, "Failed to execute command: %s\n", err)
		return
	}
	// Convert the command's output bytes to a string.
	outputStr := string(output)

	// Split the output string by new lines to get individual container names.
	containerNames := strings.Split(strings.TrimSpace(outputStr), "\n")

	// Print each container name to the terminal.
	fmt.Println("Running containers:")
	for _, name := range containerNames {
		fmt.Println(name)
	}

	cmd = exec.Command("docker", "logs", "integration-tests-kafka-gen-1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Stderr.WriteString(err.Error())
	}
	fmt.Println("Cleaning up Docker environment...")
	err = exec.Command("docker-compose", "down", "--volumes", "--rmi", "all").Run()
	if err != nil {
		panic(err)
	}
}

func reportResults() {
	testsFailed := 0
	// It's ok to close the channel here because all tests are finished.
	// If the channel would not be closed, the for loop would wait forever.
	close(logChan)
	for log := range logChan {
		if strings.Contains(log.TestOutput, "build constraints exclude all Go files") {
			fmt.Printf("Test %q is not applicable for this OS, ignoring\n", log.TestDir)
			continue
		}
		fmt.Printf("Failure detected in %s:\n", log.TestDir)
		fmt.Println("Test output:", log.TestOutput)
		fmt.Println("Agent logs:", log.AgentLog)
		testsFailed++
	}

	if testsFailed > 0 {
		fmt.Printf("%d tests failed!\n", testsFailed)
		os.Exit(1)
	} else {
		fmt.Println("All integration tests passed!")
	}
}
