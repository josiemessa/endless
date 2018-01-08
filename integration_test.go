package endless

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

const (
	integrationServerFeatureToggle = "GO_INTEGRATION_TEST_SERVER"
	integrationServerPort          = "4242"
	integrationServerPath          = "/test"
)

var (
	serverpids = make(map[int]struct{})
)

// TestIntegrationServer isn't a real test - instead it is used as a
// test server binary so that we can test signal handling and the subsequent
// shutdown/restarts from a separate binary from the test itself.
// This test is only run when an environment variable feature toggle
// (named by integrationServerFeatureToggle) is set by the calling test
func TestIntegrationServer(t *testing.T) {
	if os.Getenv(integrationServerFeatureToggle) != "1" {
		t.Skip("Skipping as have not been asked to stand up integration server")
	}
	mux1 := mux.NewRouter()

	mux1.HandleFunc(integrationServerPath, integrationTestServerHandler).Methods("GET")

	srv := NewServer("tcp", "localhost:"+integrationServerPort, mux1)
	srv.ErrorLog = log.New(os.Stdout, "", log.LstdFlags)
	srv.Debug = true
	srv.TerminateTimeout = 5 * time.Second

	// This is used to signal back to the calling test that the server has started up
	fmt.Println("PID:", os.Getpid())
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println(err)
	}
	fmt.Println("Shutting down")
}

// handler used by the integration test server
func integrationTestServerHandler(w http.ResponseWriter, r *http.Request) {
	duration, err := time.ParseDuration(r.FormValue("duration"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	time.Sleep(duration)
	fmt.Fprintf(
		w, "Slept for %2v seconds. Response from pid %d.\n",
		duration.Seconds(),
		os.Getpid(),
	)
}

// createIntegrationServer will re-run the test binary that is currently running,
// but only execute the "TestIntegrationServer" test, which will stand up an
// 'endless' server which can be used to test signal handling
func createIntegrationServerTest() *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestIntegrationServer")

	// Set the feature toggle for the integration test server
	cmd.Env = []string{integrationServerFeatureToggle + "=1"}

	return cmd
}

func TestIntegrationRestart(t *testing.T) {
	intServer := createIntegrationServerTest()
	serverMessages := make(chan string)
	// Start test integration server
	require.NoError(t, startIntegrationServer(intServer, serverMessages), "starting integration test server")
	parentPid := intServer.Process.Pid
	defer teardownServer(parentPid)
	go intServer.Wait()

	// The pid is printed out from the integation test server once it has fully started up
	// Check for this message so we can start sending requests/signals to the server
	serverStartTimeout := time.NewTimer(3 * time.Second)
	select {
	case <-serverStartTimeout.C:
		t.Fatal("Server did not start up in time")
	case msg, msgChannelOpen := <-serverMessages:
		require.True(t, msgChannelOpen, "Unexpected close of server messages channel")
		msgPid, _ := strconv.Atoi(msg) // don't care about error here as we're just going to compare msgPid to the parentpid anyway
		require.Equal(t, parentPid, msgPid, "Unexpected message down server messages channel")
	}

	time.Sleep(50 * time.Millisecond) // quick sleep just to give the server a chance to bind to port

	// Send a long running request to the test server
	longReqWait := &sync.WaitGroup{}
	longReqWait.Add(1)
	var (
		longResp     *http.Response
		longReqError error
	)
	go func() {
		longResp, longReqError = sendIntegrationTestRequest(2 * time.Second)
		longReqWait.Done()
	}()

	time.Sleep(50 * time.Millisecond) // quick sleep just to give the request a chance to get handled

	// Send a signal to the test server to restart
	require.NoError(t, intServer.Process.Signal(syscall.SIGHUP), "send signal to restart integration test server")

	// The child server should also print it's pid out once it's started up, so look for this
	var childPid int
	childStartTimeout := time.NewTimer(3 * time.Second)
	select {
	case <-childStartTimeout.C:
		t.Fatal("Child server did not start up in time")
	case msg, msgChannelOpen := <-serverMessages:
		require.True(t, msgChannelOpen, "Unexpected close of server messages channel")
		msgPid, _ := strconv.Atoi(msg) // don't care about error here as we're just going to check msgPid isn't zero
		require.NotZero(t, msgPid, "Unexpected message down server messages channel")
		childPid = msgPid
	}
	defer teardownServer(childPid) // set up the child server to be torn down on test exit

	quickResp, quickReqError := sendIntegrationTestRequest(0 * time.Second)
	require.NoError(t, quickReqError, "sending quick request to child integration test server")
	require.Equal(t,
		http.StatusOK,
		quickResp.StatusCode,
		"http response code from long running request")

	quickRespBody, err := ioutil.ReadAll(quickResp.Body)
	quickResp.Body.Close()
	require.NoError(t, err, "read from quick request body")
	require.NotContains(t,
		string(quickRespBody),
		fmt.Sprintf("%d", parentPid),
		"response from quick request contained PID from parent integration test server instead of child")
	require.Contains(t,
		string(quickRespBody),
		fmt.Sprintf("%d", childPid),
		"response from quick request did not contain PID from child integration test server")

	// Wait for long request to finish, which should be responded to from
	// the parent integration test server
	longReqWait.Wait()
	require.NoError(t,
		longReqError,
		"sending long request to parent integration test server")
	require.Equal(t,
		http.StatusOK,
		longResp.StatusCode,
		"http response code from long running request")
	longRespBody, err := ioutil.ReadAll(longResp.Body)
	require.NoError(t,
		err,
		"read from long running request body")
	require.Contains(t,
		string(longRespBody),
		fmt.Sprintf("%d", parentPid),
		"response from long running request did not contain PID from parent integration test server")
	time.Sleep(5 * time.Second)
	require.Error(t, intServer.Process.Signal(syscall.Signal(0)), "Expected error when signalling stopped parent process, process has not exited")

}

func sendIntegrationTestRequest(sleepDuration time.Duration) (*http.Response, error) {
	reqURL := fmt.Sprintf("http://localhost:%s%s?duration=%s", integrationServerPort, integrationServerPath, sleepDuration)
	timeout := int(sleepDuration.Seconds() + 2) // add two seconds to the duration of the call to get the http timeout

	client := http.Client{
		Timeout: time.Duration(timeout) * time.Second,
	}
	return client.Get(reqURL)
}

func startIntegrationServer(intServer *exec.Cmd, messages chan string) error {
	outPipe, err := intServer.StdoutPipe()
	if err != nil {
		close(messages)
		return err // note process not started yet so we shouldn't have to kill intserver
	}
	if err := intServer.Start(); err != nil {
		close(messages)
		return err
	}
	// Watch Stdout for IntegrationServerMagicWords message
	// which is sent when server shutdown has started
	go watchStdout(outPipe, messages)
	return nil
}

func teardownServer(pid int) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("Could not find process with PID %d to clean up\n", pid)
	}
	if err := proc.Kill(); err != nil {
		if !strings.Contains(err.Error(), "process already finished") {
			fmt.Printf(
				"Could not kill process with PID %d with err %v. Manual clean up may be required\n",
				pid, err)
		}
	}
}

// watchStdout watches stdout from the test integration server for specific messages
//
// * when a child server spins up, it prints out the message: "PID: <PID>", which we use
//   to tear down the child server when the test finishes
func watchStdout(outPipe io.ReadCloser, messages chan string) {
	buffer := make([]byte, 1024)
	var currentline []byte
	for {
		n, err := outPipe.Read(buffer)
		if err == io.EOF {
			fmt.Println("stdout pipe closed")
			close(messages)
			return
		}
		// As we read from the stdout pipe, there's no guarantee we'll get
		// new lines in separate reads, so keep track of what we've read already
		// and only pass in one line at a time to be checked
		for _, char := range buffer[0:n] {
			if string(char) == "\n" {
				linestring := string(currentline)
				fmt.Println(linestring)
				currentline = []byte{}
				if pid, ok := checkforpid(linestring); ok {
					messages <- pid
					break
				}
			} else {
				currentline = append(currentline, char)
			}
		}

	}

}

func checkforpid(line string) (string, bool) {
	if !strings.Contains(line, "PID: ") {
		return "", false
	}
	pid := strings.TrimPrefix(strings.TrimSpace(line), "PID: ")
	_, err := strconv.Atoi(pid)
	if err != nil {
		return "", false
	}
	return pid, true
}
