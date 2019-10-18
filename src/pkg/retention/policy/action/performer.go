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

package action

import (
	"github.com/goharbor/harbor/src/pkg/art"
	"github.com/goharbor/harbor/src/pkg/retention/dep"
)

const (
	// Retain artifacts
	Retain = "retain"
)

// Performer performs the related actions targeting the candidates
type Performer interface {
	// Perform the action
	//
	//  Arguments:
	//    candidates []*art.Candidate : the targets to perform
	//
	//  Returns:
	//    []*art.Result : result infos
	//    error     : common error if any errors occurred
	Perform(candidates []*art.Candidate) ([]*art.Result, error)
}

// PerformerFactory is factory method for creating Performer
type PerformerFactory func(params interface{}, isDryRun bool) Performer

// retainAction make sure all the candidates will be retained and others will be cleared
type retainAction struct {
	all []*art.Candidate
	// Indicate if it is a dry run
	isDryRun bool
}

// Perform the action
func (ra *retainAction) Perform(candidates []*art.Candidate) (results []*art.Result, err error) {
	retained := make(map[string]bool)
	for _, c := range candidates {
		retained[c.Hash()] = true
	}

	// start to delete
	if len(ra.all) > 0 {
		for _, c := range ra.all {
			if _, ok := retained[c.Hash()]; !ok {
				result := &art.Result{
					Target: c,
				}

				if !ra.isDryRun {
					if err := dep.DefaultClient.Delete(c); err != nil {
						result.Error = err
					}
				}

				results = append(results, result)
			}
		}
	}

	return
}

// NewRetainAction is factory method for RetainAction
func NewRetainAction(params interface{}, isDryRun bool) Performer {
	if params != nil {
		if all, ok := params.([]*art.Candidate); ok {
			return &retainAction{
				all:      all,
				isDryRun: isDryRun,
			}
		}
	}

	return &retainAction{
		all:      make([]*art.Candidate, 0),
		isDryRun: isDryRun,
	}
}
