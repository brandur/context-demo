package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"time"

	"github.com/pkg/errors"
)

const testServerURL = "http://localhost:8788"

const (
	testRecordName = "context.mutelight.org"
	testRecordType = "CNAME"
	testZoneName = "mutelight.org"
)

func makeRequest() error {
	params := &putRecordParams{RecordType: RecordTypeCNAME}

	data, err := json.Marshal(&params)
	if err != nil {
		return errors.Wrap(err, "error encoding parameters")
	}

	reader := &slowReader{
		Data: data,
		TargetDuration: targetSlowDuration,
	}

	url := fmt.Sprintf("%s/zones/%s/records/%s",
		testServerURL, testZoneName, testRecordName)

	req, err := http.NewRequest(http.MethodPut, url, reader)
	if err != nil {
		return errors.Wrap(err, "error initializing request")
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "error making request")
	}

	respData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "error reading response")
	}
	resp.Body.Close()

	fmt.Printf("response = %s", string(respData))

	return nil
}

func main() {
	err := makeRequest()
	if err != nil {
		fmt.Fprintf(os.Stderr, err.Error())
		os.Exit(1)
	}
}

//
// Helpers
//

// Constants for common record types.
const (
	RecordTypeCNAME RecordType = "CNAME"
)

const targetSlowDuration = 3 * time.Second

// RecordType is the type of a DNS record (e.g. A, CNAME).
type RecordType string

type putRecordParams struct {
	RecordType RecordType `json:"type"`
}

type slowReader struct {
	Data []byte
	TargetDuration  time.Duration

	pos int
}

func (r *slowReader) Read(data []byte) (int, error) {
	if r.pos >= len(r.Data) {
		return 0, io.EOF
	}

	if len(data) < 1 {
		return 0, fmt.Errorf("cannot reading into zero-byte slice")
	}

	timePerByte := time.Duration(int64(r.TargetDuration) / int64(len(r.Data)))
	time.Sleep(timePerByte)
	
	data[0] = r.Data[r.pos]

	fmt.Printf("read one byte slowly (slept %v) (pos %v) (%s)\n",
		timePerByte, r.pos, string(data[0]))

	r.pos = r.pos + 1
	return 1, nil
}