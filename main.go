// Copyright 2018 TriggerMesh, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/triggermesh/aws-custom-runtime/pkg/events/apiGateway"
)

type message struct {
	id         string
	deadline   int64
	data       []byte
	statusCode int
}

type responseWrapper struct {
	http.ResponseWriter
	StatusCode int
	Body       []byte
}

// Specification is a set of env variables that can be used to configure runtime API
type Specification struct {
	// Number of bootstrap processes
	NumberOfinvokers int `envconfig:"invoker_count" default:"4"`
	// Request body size limit, Mb
	RequestSizeLimit int64 `envconfig:"request_size_limit" default:"5"`
	// Funtions deadline, seconds
	FunctionTTL int64 `envconfig:"function_ttl" default:"10"`
	// Lambda runtime API port for functions
	InternalAPIport string `envconfig:"internal_api_port" default:"80"`
	// Lambda API port to put function requests and get results
	ExternalAPIport string `envconfig:"external_api_port" default:"8080"`
	// Either return function result "as is" or consider it as API Gateway JSON
	EventType string `envconfig:"event_type"`
}

var (
	// numberOfinvokers = 4
	// requestSizeLimit int64 = 5
	// functionTTL      int64 = 10

	tasks   chan message
	results map[string]chan message

	mutex sync.RWMutex

	awsEndpoint = "/2018-06-01/runtime"
	environment = map[string]string{
		"PATH":                   "/usr/local/bin:/usr/bin/:/bin:/opt/bin",
		"LD_LIBRARY_PATH":        "/lib64:/usr/lib64:$LAMBDA_RUNTIME_DIR:$LAMBDA_RUNTIME_DIR/lib:$LAMBDA_TASK_ROOT:$LAMBDA_TASK_ROOT/lib:/opt/lib",
		"AWS_LAMBDA_RUNTIME_API": "127.0.0.1",

		// Some dummy values
		"AWS_LAMBDA_FUNCTION_NAME":        "foo",
		"AWS_LAMBDA_FUNCTION_MEMORY_SIZE": "128",
		"AWS_LAMBDA_FUNCTION_VERSION":     "0.0.1",
		"AWS_LAMBDA_LOG_GROUP_NAME":       "foo-group",
		"AWS_LAMBDA_LOG_STREAM_NAME":      "foo-stream",
	}
)

func (rw *responseWrapper) Write(data []byte) (int, error) {
	rw.Body = data
	return len(data), nil
}

func (rw *responseWrapper) WriteHeader(statusCode int) {
	rw.StatusCode = statusCode
}

func setupEnv() error {
	environment["_HANDLER"], _ = os.LookupEnv("_HANDLER")
	environment["LAMBDA_TASK_ROOT"], _ = os.LookupEnv("LAMBDA_TASK_ROOT")

	for k, v := range environment {
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Specification) newTask(w http.ResponseWriter, r *http.Request) {
	requestSizeLimitInBytes := s.RequestSizeLimit * 1e+6
	functionTTLInNanoSeconds := s.FunctionTTL * 1e+9
	body, err := ioutil.ReadAll(http.MaxBytesReader(w, r.Body, requestSizeLimitInBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	now := time.Now().UnixNano()
	task := message{
		id:       fmt.Sprintf("%d", now),
		deadline: now + functionTTLInNanoSeconds,
		data:     body,
	}
	log.Printf("<- %s %s\n", task.id, task.data)

	resultsChannel := make(chan message)
	mutex.Lock()
	results[task.id] = resultsChannel
	mutex.Unlock()
	defer close(resultsChannel)

	tasks <- task

	select {
	case <-time.After(time.Duration(functionTTLInNanoSeconds)):
		log.Printf("-> ! %s Deadline is reached\n", task.id)
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(fmt.Sprintf("Deadline is reached, data %s", task.data)))
	case result := <-resultsChannel:
		log.Printf("-> %s %d %s\n", result.id, result.statusCode, result.data)
		w.WriteHeader(result.statusCode)
		w.Write(result.data)
	}
	mutex.Lock()
	delete(results, task.id)
	mutex.Unlock()
	return
}

func getTask(w http.ResponseWriter, r *http.Request) {
	task := <-tasks

	// Dummy headers required by Rust client. Replace with something meaningful
	w.Header().Set("Lambda-Runtime-Aws-Request-Id", task.id)
	w.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.Itoa(int(task.deadline)))
	w.Header().Set("Lambda-Runtime-Invoked-Function-Arn", "arn:aws:lambda:us-east-1:123456789012:function:custom-runtime")
	w.Header().Set("Lambda-Runtime-Trace-Id", "0")

	w.WriteHeader(http.StatusOK)
	w.Write(task.data)
	return
}

func initError(w http.ResponseWriter, r *http.Request) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalln(err)
	}
	defer r.Body.Close()

	log.Fatalf("Runtime initialization error: %s\n", data)
	return
}

func parsePath(query string) (string, string, error) {
	path := strings.TrimPrefix(query, awsEndpoint+"/invocation/")
	request := strings.Split(path, "/")
	if len(request) != 2 {
		return "", "", fmt.Errorf("Incorrect URL query size")
	}
	return request[0], request[1], nil
}

func responseHandler(w http.ResponseWriter, r *http.Request) {
	id, kind, err := parsePath(r.URL.Path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(err.Error()))
		return
	}

	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("! %s\n", err)
		return
	}
	defer r.Body.Close()

	mutex.RLock()
	resultsChannel, ok := results[id]
	mutex.RUnlock()
	if !ok {
		w.WriteHeader(http.StatusGone)
		w.Write([]byte("Function deadline is reached"))
		return
	}

	statusCode := 200

	switch kind {
	case "response":
	case "error":
		statusCode = 500
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf("Unknown endpoint: %s", kind)))
		return
	}
	resultsChannel <- message{
		id:         id,
		data:       data,
		statusCode: statusCode,
	}
	w.WriteHeader(http.StatusAccepted)
	return
}

func (s *Specification) mapEvent(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := responseWrapper{w, 200, []byte{}}
		switch s.EventType {
		case "API_GATEWAY":
			apiGateway.Request(r)
			h.ServeHTTP(&rw, r)
			apiGateway.Response(w, rw.StatusCode, rw.Body)
		default:
			h.ServeHTTP(w, r)
		}
	})
}

func ping(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("pong"))
	return
}

func (s *Specification) api() error {
	apiRouter := http.NewServeMux()
	apiRouter.HandleFunc(awsEndpoint+"/init/error", initError)
	apiRouter.HandleFunc(awsEndpoint+"/invocation/next", getTask)
	apiRouter.HandleFunc(awsEndpoint+"/invocation/", responseHandler)
	apiRouter.HandleFunc("/2018-06-01/ping", ping)
	return http.ListenAndServe(":"+s.InternalAPIport, apiRouter)
}

func main() {
	tasks = make(chan message, 100)
	results = make(map[string]chan message)
	defer close(tasks)

	var spec Specification

	err := envconfig.Process("", &spec)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%+v\n", spec)

	log.Println("Setup app env")
	if err := setupEnv(); err != nil {
		log.Fatalln(err)
	}

	log.Println("Starting API")
	go func() {
		log.Fatalln(spec.api())
	}()

	for i := 0; i < spec.NumberOfinvokers; i++ {
		log.Println("Starting bootstrap", i+1)
		go func(i int) {
			cmd := exec.Command("sh", "-c", environment["LAMBDA_TASK_ROOT"]+"/bootstrap")
			cmd.Env = append(os.Environ(), fmt.Sprintf("BOOTSTRAP_INDEX=%d", i))
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Fatalln(err)
			}
		}(i)
	}

	taskRouter := http.NewServeMux()
	taskHandler := http.HandlerFunc(spec.newTask)
	taskRouter.Handle("/", spec.mapEvent(taskHandler))
	log.Println("Listening...")
	log.Fatalln(http.ListenAndServe(":"+spec.ExternalAPIport, taskRouter))
}
