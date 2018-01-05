package endless

import (
	"fmt"
	"io"
	"io/ioutil"
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
	IntegrationServerFeatureToggle = "GO_INTEGRATION_TEST_SERVER"
	IntegrationServerPort          = "4242"
	IntegrationServerPath          = "/test"
)

var (
	serverpids = make(map[int]struct{})
)

// TestIntegrationServer isn't a real test - instead it is used as a
// test server binary so that we can test signal handling and the subsequent
// shutdown/restarts from a separate binary from the test itself.
// This test is only run when an environment variable feature toggle
// (named by IntegrationServerFeatureToggle) is set by the calling test
func TestIntegrationServer(t *testing.T) {
	if os.Getenv(IntegrationServerFeatureToggle) != "1" {
		t.Skip("Skipping as do not need to stand up integration test server in this run")
	}

	// Create new HTTP server
	mux1 := mux.NewRouter()

	mux1.HandleFunc(IntegrationServerPath, integrationTestServerHandler).
		Methods("GET")

	srv := NewServer("tcp", "localhost:"+IntegrationServerPort, mux1)
	srv.Debug = true

	// This is used to signal back to the calling test that the server has started up
	fmt.Println("PID:", os.Getpid())
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println(err)
	}
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
	cmd.Env = []string{IntegrationServerFeatureToggle + "=1"}

	return cmd
}

func TestIntegrationRestart(t *testing.T) {

	intServer := createIntegrationServerTest()
	serverMessages := make(chan string) // channel to receive notifications from the integration server
	// Start test integration server
	require.NoError(t, startIntegrationServer(intServer, serverMessages), "starting integration test server")
	parentPid := intServer.Process.Pid
	defer teardownServer(parentPid)
	go intServer.Wait() // when this ends the parent process should be over and pipe clean up will happen

	// The pid is printed out from the integation test server once it has fully started up
	// Check for this message so we can start sending requests/signals to the server
	msg, msgChannelOpen := <-serverMessages
	require.True(t, msgChannelOpen, "Unexpected close of server messages channel")
	msgPid, _ := strconv.Atoi(msg)
	require.Equal(t, parentPid, msgPid, "Unexpected message down server messages channel")
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
	msg, msgChannelOpen = <-serverMessages
	require.True(t, msgChannelOpen, "Unexpected close of server messages channel")
	msgPid, _ = strconv.Atoi(msg)
	require.NotZero(t, msgPid, "Unexpected message down server messages channel")
	childPid := msgPid
	defer teardownServer(childPid) // set up the child server to be torn down on test exit

	quickResp, quickReqError := sendIntegrationTestRequest(0 * time.Second)
	require.NoError(t, quickReqError, "sending quick request to child integration test server")
	require.Equal(t,
		http.StatusOK,
		quickResp.StatusCode,
		"http response code from long running request")

	quickRespBody, err := ioutil.ReadAll(quickResp.Body)
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
}

func sendIntegrationTestRequest(sleepDuration time.Duration) (*http.Response, error) {
	reqURL := fmt.Sprintf("http://localhost:%s%s?duration=%s", IntegrationServerPort, IntegrationServerPath, sleepDuration)
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
		teardownServer(intServer.Process.Pid)
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
	for {
		n, err := outPipe.Read(buffer)
		if err == io.EOF {
			fmt.Println("stdout pipe closed")
			close(messages)
			return
		}
		buffer = buffer[0:n]
		line := strings.TrimSpace(string(buffer))
		fmt.Println(line)
		if strings.HasPrefix(line, "PID: ") {
			pid := strings.TrimPrefix(line, "PID: ")
			_, err := strconv.Atoi(pid)
			if err != nil {
				continue
			}
			messages <- pid
		}
	}
}
