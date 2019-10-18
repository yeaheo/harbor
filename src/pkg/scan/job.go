// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/goharbor/harbor/src/jobservice/job"
	"github.com/goharbor/harbor/src/jobservice/logger"
	"github.com/goharbor/harbor/src/pkg/scan/dao/scanner"
	"github.com/goharbor/harbor/src/pkg/scan/report"
	v1 "github.com/goharbor/harbor/src/pkg/scan/rest/v1"
	"github.com/pkg/errors"
)

const (
	// JobParamRegistration ...
	JobParamRegistration = "registration"
	// JobParameterRequest ...
	JobParameterRequest = "scanRequest"
	// JobParameterMimes ...
	JobParameterMimes = "mimeTypes"

	checkTimeout       = 30 * time.Minute
	firstCheckInterval = 2 * time.Second
)

// CheckInReport defines model for checking in the scan report with specified mime.
type CheckInReport struct {
	Digest           string `json:"digest"`
	RegistrationUUID string `json:"registration_uuid"`
	MimeType         string `json:"mime_type"`
	RawReport        string `json:"raw_report"`
}

// FromJSON parse json to CheckInReport
func (cir *CheckInReport) FromJSON(jsonData string) error {
	if len(jsonData) == 0 {
		return errors.New("empty JSON data")
	}

	return json.Unmarshal([]byte(jsonData), cir)
}

// ToJSON marshal CheckInReport to JSON
func (cir *CheckInReport) ToJSON() (string, error) {
	jsonData, err := json.Marshal(cir)
	if err != nil {
		return "", errors.Wrap(err, "To JSON: CheckInReport")
	}

	return string(jsonData), nil
}

// Job for running scan in the job service with async way
type Job struct{}

// MaxFails for defining the number of retries
func (j *Job) MaxFails() uint {
	return 3
}

// ShouldRetry indicates if the job should be retried
func (j *Job) ShouldRetry() bool {
	return true
}

// Validate the parameters of this job
func (j *Job) Validate(params job.Parameters) error {
	if params == nil {
		// Params are required
		return errors.New("missing parameter of scan job")
	}

	if _, err := extractRegistration(params); err != nil {
		return errors.Wrap(err, "job validate")
	}

	if _, err := ExtractScanReq(params); err != nil {
		return errors.Wrap(err, "job validate")
	}

	if _, err := extractMimeTypes(params); err != nil {
		return errors.Wrap(err, "job validate")
	}

	return nil
}

// Run the job
func (j *Job) Run(ctx job.Context, params job.Parameters) error {
	// Get logger
	myLogger := ctx.GetLogger()

	// Ignore errors as they have been validated already
	r, _ := extractRegistration(params)
	req, _ := ExtractScanReq(params)
	mimes, _ := extractMimeTypes(params)

	// Print related infos to log
	printJSONParameter(JobParamRegistration, params[JobParamRegistration].(string), myLogger)
	printJSONParameter(JobParameterRequest, removeAuthInfo(req), myLogger)
	myLogger.Infof("Report mime types: %v\n", mimes)

	// Submit scan request to the scanner adapter
	client, err := v1.DefaultClientPool.Get(r)
	if err != nil {
		return logAndWrapError(myLogger, err, "scan job: get client")
	}

	resp, err := client.SubmitScan(req)
	if err != nil {
		return logAndWrapError(myLogger, err, "scan job: submit scan request")
	}

	// For collecting errors
	errs := make([]error, len(mimes))

	// Concurrently retrieving report by different mime types
	wg := &sync.WaitGroup{}
	wg.Add(len(mimes))

	for i, mt := range mimes {
		go func(i int, m string) {
			defer wg.Done()

			// Log info
			myLogger.Infof("Get report for mime type: %s", m)

			// Loop check if the report is ready
			tm := time.NewTimer(firstCheckInterval)
			defer tm.Stop()

			for {
				select {
				case t := <-tm.C:
					myLogger.Debugf("check scan report for mime %s at %s", m, t.Format("2006/01/02 15:04:05"))

					rawReport, err := client.GetScanReport(resp.ID, m)
					if err != nil {
						// Not ready yet
						if notReadyErr, ok := err.(*v1.ReportNotReadyError); ok {
							// Reset to the new check interval
							tm.Reset(time.Duration(notReadyErr.RetryAfter) * time.Second)
							myLogger.Infof("Report with mime type %s is not ready yet, retry after %d seconds", m, notReadyErr.RetryAfter)

							continue
						}

						errs[i] = errors.Wrap(err, fmt.Sprintf("check scan report with mime type %s", m))
						return
					}

					// Make sure the data is aligned with the v1 spec.
					if _, err = report.ResolveData(m, []byte(rawReport)); err != nil {
						errs[i] = errors.Wrap(err, "scan job: resolve report data")
						return
					}

					// Check in
					cir := &CheckInReport{
						Digest:           req.Artifact.Digest,
						RegistrationUUID: r.UUID,
						MimeType:         m,
						RawReport:        rawReport,
					}

					var (
						jsonData string
						er       error
					)
					if jsonData, er = cir.ToJSON(); er == nil {
						if er = ctx.Checkin(jsonData); er == nil {
							// Done!
							myLogger.Infof("Report with mime type %s is checked in", m)
							return
						}
					}

					// Send error and exit
					errs[i] = errors.Wrap(er, fmt.Sprintf("check in scan report for mime type %s", m))
					return
				case <-ctx.SystemContext().Done():
					// Terminated by system
					return
				case <-time.After(checkTimeout):
					errs[i] = errors.New("check scan report timeout")
					return
				}
			}
		}(i, mt)
	}

	// Wait for all the retrieving routines are completed
	wg.Wait()

	// Merge errors
	for _, e := range errs {
		if e != nil {
			if err != nil {
				err = errors.Wrap(e, err.Error())
			} else {
				err = e
			}
		}
	}

	// Log error to the job log
	if err != nil {
		myLogger.Error(err)
	}

	return err
}

// ExtractScanReq extracts the scan request from the job parameters.
func ExtractScanReq(params job.Parameters) (*v1.ScanRequest, error) {
	v, ok := params[JobParameterRequest]
	if !ok {
		return nil, errors.Errorf("missing job parameter '%s'", JobParameterRequest)
	}

	jsonData, ok := v.(string)
	if !ok {
		return nil, errors.Errorf(
			"malformed job parameter '%s', expecting string but got %s",
			JobParameterRequest,
			reflect.TypeOf(v).String(),
		)
	}

	req := &v1.ScanRequest{}
	if err := req.FromJSON(jsonData); err != nil {
		return nil, err
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}

	return req, nil
}

func logAndWrapError(logger logger.Interface, err error, message string) error {
	e := errors.Wrap(err, message)
	logger.Error(e)

	return e
}

func printJSONParameter(parameter string, v string, logger logger.Interface) {
	logger.Infof("%s:\n", parameter)
	printPrettyJSON([]byte(v), logger)
}

func printPrettyJSON(in []byte, logger logger.Interface) {
	var out bytes.Buffer
	if err := json.Indent(&out, in, "", "  "); err != nil {
		logger.Errorf("Print pretty JSON error: %s", err)
		return
	}

	logger.Infof("%s\n", out.String())
}

func removeAuthInfo(sr *v1.ScanRequest) string {
	req := &v1.ScanRequest{
		Artifact: sr.Artifact,
		Registry: &v1.Registry{
			URL:           sr.Registry.URL,
			Authorization: "[HIDDEN]",
		},
	}

	str, err := req.ToJSON()
	if err != nil {
		logger.Error(errors.Wrap(err, "scan job: remove auth"))
	}

	return str
}

func extractRegistration(params job.Parameters) (*scanner.Registration, error) {
	v, ok := params[JobParamRegistration]
	if !ok {
		return nil, errors.Errorf("missing job parameter '%s'", JobParamRegistration)
	}

	jsonData, ok := v.(string)
	if !ok {
		return nil, errors.Errorf(
			"malformed job parameter '%s', expecting string but got %s",
			JobParamRegistration,
			reflect.TypeOf(v).String(),
		)
	}

	r := &scanner.Registration{}
	if err := r.FromJSON(jsonData); err != nil {
		return nil, err
	}

	if err := r.Validate(true); err != nil {
		return nil, err
	}

	return r, nil
}

func extractMimeTypes(params job.Parameters) ([]string, error) {
	v, ok := params[JobParameterMimes]
	if !ok {
		return nil, errors.Errorf("missing job parameter '%s'", JobParameterMimes)
	}

	l, ok := v.([]interface{})
	if !ok {
		return nil, errors.Errorf(
			"malformed job parameter '%s', expecting []interface{} but got %s",
			JobParameterMimes,
			reflect.TypeOf(v).String(),
		)
	}

	mimes := make([]string, 0)
	for _, v := range l {
		mime, ok := v.(string)
		if !ok {
			return nil, errors.Errorf("expect string but got %s", reflect.TypeOf(v).String())
		}

		mimes = append(mimes, mime)
	}

	return mimes, nil
}
