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

	"github.com/stretchr/testify/assert"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

const (
	IntegrationServerFeatureToggle = "GO_INTEGRATION_TEST_SERVER"
	IntegrationServerPort          = "4242"
	IntegrationServerPath          = "/test"
	IntegrationServerMagicWords    = "Shutdown started"
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

	fmt.Println("PID:", os.Getpid())

	// Once the server receives a request to shut down,
	// print out the magic words so that the calling test can be notified
	go func() {
		done := make(chan interface{})
		go watchForShutdown(srv, done)
		<-done
		fmt.Println("shut down completed")
		// Server shut down initiated, notify the integration test client
		// so it can send a request to the newly forked child integration test server
		fmt.Println(IntegrationServerMagicWords)
	}()
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println(err)
	}
}

// handler used by the integration test server
func integrationTestServerHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Request received")
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

// wait for the server to enter the "shutting down" state, used by the main test
// server to notify the calling test that shutdown has begun
func watchForShutdown(srv *Server, done chan interface{}) {
	// if it's not starting shut down in 5 seconds we're buggered
	for index := 0; index < 100; index++ {
		if state := srv.GetState(); state == StateShuttingDown {
			fmt.Println("wibble")
			close(done)
			return
		}

		// Try not to kill the server completely by deadlocking it
		// As GetState requests the lock every time
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Println("ruh roh")
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
	endTest := make(chan interface{})
	intServer := createIntegrationServerTest()
	// Create channel to receive notifications from the integration server
	serverMessages := make(chan string)

	fmt.Println("Stating integration server")
	// Start test integration server
	go startIntegrationServer(intServer, serverMessages)
	time.Sleep(1 * time.Second)

	// Send a long running request to the test server
	longRequestWait := sync.WaitGroup{}
	longRequestWait.Add(1)
	var longResp *http.Response
	go func() {
		var err error
		reqURL := fmt.Sprintf("http://localhost:%s%s?duration=5s", IntegrationServerPort, IntegrationServerPath)
		fmt.Println("Sending long running request to integration test server")
		client := http.Client{
			Timeout: 10 * time.Second,
		}
		longResp, err = client.Get(reqURL)
		if !assert.NoError(t, err, "sending long running request to integration test server") {
			fmt.Println("HTTP error, closing endTest channel")
			close(endTest)
		}
		longRequestWait.Done()
	}()
	// Wait for the child server to come up, which notifies the parent server to begin shutdown,
	// which causes the messages channel to be closed
	select {
	case <-endTest:
		teardownServer(intServer.Process.Pid)
		t.FailNow()
	default:
	}
	intServerWait := sync.WaitGroup{}
	intServerWait.Add(1)
	var childPid int
	go func() {
		timeout := time.NewTimer(5 * time.Second)
		for {
			select {
			case <-endTest:
				fmt.Println("Something else requested to end test")

				intServerWait.Done()
				return
			case msg, ok := <-serverMessages:
				// this channel is closed when the server has begun shutdown
				if !ok {
					intServerWait.Done()
					return
				}
				pid, _ := strconv.Atoi(msg) // should not be error as we checked earlier (TODO: Update this incase we used messages for anything else)
				// The pid is printed out from the integation test server once it has fully started up so we know it should exist by now
				if intServer.Process.Pid == pid {
					// Send a signal to the test server to restart
					fmt.Println("Sending SIGHUP to integration test server")
					if !assert.NoError(t, intServer.Process.Signal(syscall.SIGHUP), "send signal to restart integration test server") {
						close(endTest)
						intServerWait.Done()
					}
				}
				childPid = pid
			case <-timeout.C:
				fmt.Println("Timeout reached")
				close(endTest)
				intServerWait.Done()
				return
			}
		}
	}()
	select {
	case <-endTest:
		teardownServer(intServer.Process.Pid)
		if childPid != 0 {
			teardownServer(childPid)
		}
		t.FailNow()
	default:
	}

	// Send a quick request to the server, which should now be running from
	// the child integration test server
	fmt.Println("Waiting for parent test integration server to begin shutdown")
	intServerWait.Wait()
	// Send a quick request to the server
	fmt.Println("Sending quick request to integration test server")
	select {
	case <-endTest:
		teardownServer(intServer.Process.Pid)
		if childPid != 0 {
			teardownServer(childPid)
		}
		t.FailNow()
	default:
	}
	quickURL := fmt.Sprintf("http://localhost:%s%s?duration=0s", IntegrationServerPort, IntegrationServerPath)
	quickResp, err := http.Get(quickURL)
	require.NoError(t, err, "sending quick request to integration test server")
	require.Equal(t, http.StatusOK, quickResp.StatusCode, "http response code from quick request")
	quickBody, err := ioutil.ReadAll(quickResp.Body)
	require.NoError(t, err, "read from quick request body")
	require.NotContains(t, quickBody, intServer.Process.Pid, "response from quick request contained PID from parent integration test server instead of child")

	select {
	case <-endTest:
		teardownServer(intServer.Process.Pid)
		if childPid != 0 {
			teardownServer(childPid)
		}
		t.FailNow()
	default:
	}
	// Wait for long request to finish, which should be responded to from
	// the parent integration test server
	fmt.Println("Waiting for long running request to complete")
	longRequestWait.Wait()
	require.Equal(t, http.StatusOK, longResp.StatusCode, "http response code from long running request")
	body, err := ioutil.ReadAll(longResp.Body)
	require.NoError(t, err, "read from long running request body")
	require.Contains(t, string(body), fmt.Sprintf("%d", intServer.Process.Pid), "response from long running request did not contain PID from parent integration test server")

}

func startIntegrationServer(intServer *exec.Cmd, messages chan string) error {
	outPipe, err := intServer.StdoutPipe()
	if err != nil {
		return err
	}
	if err := intServer.Start(); err != nil {
		return err
	}
	// Watch Stdout for IntegrationServerMagicWords message
	// which is sent when server shutdown has started
	go watchStdout(outPipe, messages)
	return intServer.Wait()
}

func teardownServer(pid int) {
	fmt.Println("Tearing down server with pid", pid)
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("Could not find process with PID %d to clean up\n", pid)
	}
	if err := proc.Kill(); err != nil {
		fmt.Printf("Could not kill process with PID %d. Manual clean up required\n", pid)
	}
}

// watchStdout watches stdout from the test integration server for specific messages
//
// * when a child server spins up, it prints out the message: "PID: <PID>", which we use
//   to tear down the child server when the test finishes
// * when the child server notifies the parent it has finished startup, the parent
//   begins its shut down process, at which point it prints out the "magic words"
//   defined in the IntegrationServerMagicWords const
func watchStdout(outPipe io.ReadCloser, messages chan string) {
	buffer := make([]byte, 1024)
	for {
		n, err := outPipe.Read(buffer)
		if err == io.EOF {
			fmt.Println("EOF")
			// close(messages)
			break // wait below will close the pipe so we don't need to here
		}
		buffer = buffer[0:n]
		line := strings.TrimSpace(string(buffer))
		if line == IntegrationServerMagicWords {
			close(messages)
			break
		}
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
