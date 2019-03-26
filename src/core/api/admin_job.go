// Copyright 2018 Project Harbor Authors
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

package api

import (
	"fmt"
	"net/http"
	"strconv"

	"encoding/json"
	"github.com/goharbor/harbor/src/common/dao"
	common_http "github.com/goharbor/harbor/src/common/http"
	common_job "github.com/goharbor/harbor/src/common/job"
	common_models "github.com/goharbor/harbor/src/common/models"
	"github.com/goharbor/harbor/src/common/utils/log"
	"github.com/goharbor/harbor/src/core/api/models"
	utils_core "github.com/goharbor/harbor/src/core/utils"
)

// AJAPI manages the CRUD of admin job and its schedule, any API wants to handle manual and cron job like ScanAll and GC cloud reuse it.
type AJAPI struct {
	BaseController
}

// Prepare validates the URL and parms, it needs the system admin permission.
func (aj *AJAPI) Prepare() {
	aj.BaseController.Prepare()
}

// updateSchedule update a schedule of admin job.
func (aj *AJAPI) updateSchedule(ajr models.AdminJobReq) {
	if ajr.Schedule.Type == models.ScheduleManual {
		aj.HandleInternalServerError(fmt.Sprintf("Fail to update admin job schedule as wrong schedule type: %s.", ajr.Schedule.Type))
		return
	}

	query := &common_models.AdminJobQuery{
		Name: ajr.Name,
		Kind: common_job.JobKindPeriodic,
	}
	jobs, err := dao.GetAdminJobs(query)
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("%v", err))
		return
	}
	if len(jobs) != 1 {
		aj.HandleInternalServerError("Fail to update admin job schedule as we found more than one schedule in system, please ensure that only one schedule left for your job .")
		return
	}

	// stop the scheduled job and remove it.
	if err = utils_core.GetJobServiceClient().PostAction(jobs[0].UUID, common_job.JobActionStop); err != nil {
		if e, ok := err.(*common_http.Error); !ok || e.Code != http.StatusNotFound {
			aj.HandleInternalServerError(fmt.Sprintf("%v", err))
			return
		}
	}

	if err = dao.DeleteAdminJob(jobs[0].ID); err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("%v", err))
		return
	}

	// Set schedule to None means to cancel the schedule, won't add new job.
	if ajr.Schedule.Type != models.ScheduleNone {
		aj.submit(&ajr)
	}
}

// get get a execution of admin job by ID
func (aj *AJAPI) get(id int64) {
	jobs, err := dao.GetAdminJobs(&common_models.AdminJobQuery{
		ID: id,
	})
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("failed to get admin jobs: %v", err))
		return
	}
	if len(jobs) == 0 {
		aj.HandleNotFound("No admin job found.")
		return
	}

	adminJobRep, err := convertToAdminJobRep(jobs[0])
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("failed to convert admin job response: %v", err))
		return
	}

	aj.Data["json"] = adminJobRep
	aj.ServeJSON()
}

// list list all executions of admin job by name
func (aj *AJAPI) list(name string) {
	jobs, err := dao.GetTop10AdminJobsOfName(name)
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("failed to get admin jobs: %v", err))
		return
	}

	AdminJobReps := []*models.AdminJobRep{}
	for _, job := range jobs {
		AdminJobRep, err := convertToAdminJobRep(job)
		if err != nil {
			aj.HandleInternalServerError(fmt.Sprintf("failed to convert admin job response: %v", err))
			return
		}
		AdminJobReps = append(AdminJobReps, &AdminJobRep)
	}

	aj.Data["json"] = AdminJobReps
	aj.ServeJSON()
}

// getSchedule gets admin job schedule ...
func (aj *AJAPI) getSchedule(name string) {
	adminJobSchedule := models.AdminJobSchedule{}

	jobs, err := dao.GetAdminJobs(&common_models.AdminJobQuery{
		Name: name,
		Kind: common_job.JobKindPeriodic,
	})
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("failed to get admin jobs: %v", err))
		return
	}
	if len(jobs) > 1 {
		aj.HandleInternalServerError("Get more than one scheduled admin job, make sure there has only one.")
		return
	}

	if len(jobs) != 0 {
		adminJobRep, err := convertToAdminJobRep(jobs[0])
		if err != nil {
			aj.HandleInternalServerError(fmt.Sprintf("failed to convert admin job response: %v", err))
			return
		}
		adminJobSchedule.Schedule = adminJobRep.Schedule
	}

	aj.Data["json"] = adminJobSchedule
	aj.ServeJSON()
}

// getLog ...
func (aj *AJAPI) getLog(id int64) {
	job, err := dao.GetAdminJob(id)
	if err != nil {
		log.Errorf("Failed to load job data for job: %d, error: %v", id, err)
		aj.CustomAbort(http.StatusInternalServerError, "Failed to get Job data")
	}
	if job == nil {
		log.Errorf("Failed to get admin job: %d", id)
		aj.CustomAbort(http.StatusNotFound, "Failed to get Job")
	}

	logBytes, err := utils_core.GetJobServiceClient().GetJobLog(job.UUID)
	if err != nil {
		if httpErr, ok := err.(*common_http.Error); ok {
			aj.RenderError(httpErr.Code, "")
			log.Errorf(fmt.Sprintf("failed to get log of job %d: %d %s",
				id, httpErr.Code, httpErr.Message))
			return
		}
		aj.HandleInternalServerError(fmt.Sprintf("Failed to get job logs, uuid: %s, error: %v", job.UUID, err))
		return
	}
	aj.Ctx.ResponseWriter.Header().Set(http.CanonicalHeaderKey("Content-Length"), strconv.Itoa(len(logBytes)))
	aj.Ctx.ResponseWriter.Header().Set(http.CanonicalHeaderKey("Content-Type"), "text/plain")
	_, err = aj.Ctx.ResponseWriter.Write(logBytes)
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("Failed to write job logs, uuid: %s, error: %v", job.UUID, err))
	}
}

// submit submits a job to job service per request
func (aj *AJAPI) submit(ajr *models.AdminJobReq) {
	// cannot post multiple schedule for admin job.
	if ajr.IsPeriodic() {
		jobs, err := dao.GetAdminJobs(&common_models.AdminJobQuery{
			Name: ajr.Name,
			Kind: common_job.JobKindPeriodic,
		})
		if err != nil {
			aj.HandleInternalServerError(fmt.Sprintf("failed to get admin jobs: %v", err))
			return
		}
		if len(jobs) != 0 {
			aj.HandleStatusPreconditionFailed("Fail to set schedule for admin job as always had one, please delete it firstly then to re-schedule.")
			return
		}
	}

	id, err := dao.AddAdminJob(&common_models.AdminJob{
		Name: ajr.Name,
		Kind: ajr.JobKind(),
		Cron: ajr.CronString(),
	})
	if err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("%v", err))
		return
	}
	ajr.ID = id
	job := ajr.ToJob()

	// submit job to job service
	log.Debugf("submitting admin job to job service")
	uuid, err := utils_core.GetJobServiceClient().SubmitJob(job)
	if err != nil {
		if err := dao.DeleteAdminJob(id); err != nil {
			log.Debugf("Failed to delete admin job, err: %v", err)
		}
		if httpErr, ok := err.(*common_http.Error); ok && httpErr.Code == http.StatusConflict {
			aj.HandleConflict(fmt.Sprintf("Conflict when triggering %s, please try again later.", ajr.Name))
			return
		}
		aj.HandleInternalServerError(fmt.Sprintf("%v", err))
		return
	}
	if err := dao.SetAdminJobUUID(id, uuid); err != nil {
		aj.HandleInternalServerError(fmt.Sprintf("%v", err))
		return
	}
}

func convertToAdminJobRep(job *common_models.AdminJob) (models.AdminJobRep, error) {
	if job == nil {
		return models.AdminJobRep{}, nil
	}

	AdminJobRep := models.AdminJobRep{
		ID:           job.ID,
		Name:         job.Name,
		Kind:         job.Kind,
		Status:       job.Status,
		CreationTime: job.CreationTime,
		UpdateTime:   job.UpdateTime,
	}

	if len(job.Cron) > 0 {
		schedule := &models.ScheduleParam{}
		if err := json.Unmarshal([]byte(job.Cron), &schedule); err != nil {
			return models.AdminJobRep{}, err
		}
		AdminJobRep.Schedule = schedule
	}
	return AdminJobRep, nil
}
