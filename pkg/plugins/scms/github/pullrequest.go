package github

/*
	Keeping the pullrequest code under the github package to avoid cyclic dependencies between github <-> pullrequest
	We need to completely refactor the github package to split the different component into specific sub packages

		github/pullrequest
		github/target
		github/scm
		github/source
		github/condition
*/

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/shurcooL/githubv4"
	"github.com/sirupsen/logrus"

	"github.com/updatecli/updatecli/pkg/core/reports"
	utils "github.com/updatecli/updatecli/pkg/plugins/utils/action"
)

var (
	ErrAutomergeNotAllowOnRepository = errors.New("automerge is not allowed on repository")
	ErrBadMergeMethod                = errors.New("wrong merge method defined, accepting one of 'squash', 'merge', 'rebase', or ''")
	ErrPullRequestIsInCleanStatus    = errors.New("Pull request Pull request is in clean status")
)

// PullRequest contains multiple fields mapped to GitHub V4 api
type PullRequestApi struct {
	ChangedFiles int
	BaseRefName  string
	Body         string
	HeadRefName  string
	ID           string
	State        string
	Title        string
	Url          string
	Number       int
}

// ActionSpec specifies the configuration of an action of type "GitHub Pull Request"
type ActionSpec struct {
	// Specifies if automerge is enabled for the new pullrequest
	AutoMerge bool `yaml:",omitempty"`
	// Specifies the Pull Request title
	Title string `yaml:",omitempty"`
	// Specifies user input description used during pull body creation
	Description string `yaml:",omitempty"`
	// Specifies repository labels used for the Pull Request. !! Labels must already exist on the repository
	Labels []string `yaml:",omitempty"`
	// Specifies if a Pull Request is set to draft, default false
	Draft bool `yaml:",omitempty"`
	// Specifies if maintainer can modify pullRequest
	MaintainerCannotModify bool `yaml:",omitempty"`
	// Specifies which merge method is used to incorporate the Pull Request. Accept "merge", "squash", "rebase", or ""
	MergeMethod string `yaml:",omitempty"`
	// Specifies to use the Pull Request title as commit message when using auto merge, only works for "squash" or "rebase"
	UseTitleForAutoMerge bool `yaml:",omitempty"`
	// Specifies if a Pull Request should be sent to the parent of a fork.
	Parent bool `yaml:",omitempty"`
}

type PullRequest struct {
	gh                *Github
	Report            string
	Title             string
	spec              ActionSpec
	remotePullRequest PullRequestApi
	repository        *Repository
}

// isMergeMethodValid ensure that we specified a valid merge method.
func isMergeMethodValid(method string) (bool, error) {
	if len(method) == 0 ||
		strings.ToUpper(method) == "SQUASH" ||
		strings.ToUpper(method) == "MERGE" ||
		strings.ToUpper(method) == "REBASE" {
		return true, nil
	}
	logrus.Debugf("%s - %s", method, ErrBadMergeMethod)
	return false, ErrBadMergeMethod
}

// Validate ensures that the provided ActionSpec is valid
func (s *ActionSpec) Validate() error {

	if _, err := isMergeMethodValid(s.MergeMethod); err != nil {
		return err
	}
	return nil
}

// Graphql mutation used with GitHub api to enable automerge on a existing
// pullrequest
type mutationEnablePullRequestAutoMerge struct {
	EnablePullRequestAutoMerge struct {
		PullRequest PullRequestApi
	} `graphql:"enablePullRequestAutoMerge(input: $input)"`
}

func NewAction(spec ActionSpec, gh *Github) (PullRequest, error) {
	err := spec.Validate()

	return PullRequest{
		gh:   gh,
		spec: spec,
	}, err
}

// CleanAction verifies if an existing action requires some cleanup such as closing a pullrequest with no changes.
func (p *PullRequest) CleanAction(report reports.Action) error {

	repository, err := p.gh.queryRepository("", "")
	if err != nil {
		return err
	}

	p.repository = repository

	// Check if there is already a pullRequest for current pipeline
	err = p.getRemotePullRequest(false)
	if err != nil {
		return err
	}

	if p.remotePullRequest.ID == "" {
		logrus.Debugln("nothing to clean")
		return nil
	}

	if p.remotePullRequest.ChangedFiles == 0 {
		logrus.Debugf("No changed file detected at pull request:\n\t%s", p.remotePullRequest.Url)
		// Not returning an error if the comment failed to be added
		// as the main purpose of this function is to close the pullrequest
		return p.closePullRequest()
	}

	return nil
}

// CreateAction creates a new GitHub Pull Request or update an existing one.
func (p *PullRequest) CreateAction(report reports.Action, resetDescription bool) error {

	// One GitHub pullrequest body can contain multiple action report
	// It would be better to refactor CreateAction
	p.Report = report.ToActionsString()
	p.Title = report.Title

	if p.spec.Title != "" {
		p.Title = p.spec.Title
	}

	sourceBranch, workingBranch, _ := p.gh.GetBranches()

	repository, err := p.gh.queryRepository(sourceBranch, workingBranch)
	if err != nil {
		return err
	}

	p.repository = repository

	// Check if there is already a pullRequest for current pipeline
	err = p.getRemotePullRequest(resetDescription)
	if err != nil {
		return err
	}

	// If we didn't find a Pull Request ID then it means we need to create a new pullrequest.
	if len(p.remotePullRequest.ID) == 0 {
		if err := p.OpenPullRequest(); err != nil {
			return err
		}
	}

	// Once the remote Pull Request exists, we can than update it with additional information such as
	// tags,assignee,etc.
	if err := p.updatePullRequest(); err != nil {
		return err
	}

	// Check if they are changes that need to be published otherwise exit
	// It's worth mentioning that at this time, changes have already been published
	// The goal is just to not open a pull request if there is no changes
	isAhead := p.isAhead()
	logrus.Debugf("Branch %s is %s of %s", workingBranch, p.repository.Status, sourceBranch)

	if !isAhead {
		logrus.Debugf("GitHub pullrequest not needed")

		return nil
	}

	// Now that the pullrequest has been updated with the new report, we can now close it if needed.
	if p.remotePullRequest.ChangedFiles == 0 {
		logrus.Debugf("No changed file detected in pull request %s", p.remotePullRequest.Url)
		// Not returning an error if the comment failed to be added
		// as the main purpose of this function is to close the pullrequest
		return p.closePullRequest()
	}

	if p.spec.AutoMerge {
		if err := p.EnablePullRequestAutoMerge(); err != nil {
			switch err.Error() {
			case ErrAutomergeNotAllowOnRepository.Error():
				logrus.Errorln("Automerge can't be enabled. Make sure to all it on the repository.")
			case ErrPullRequestIsInCleanStatus.Error():
				logrus.Errorln("Automerge can't be enabled. Make sure to have branch protection rules enabled on the repository.")
			default:
				logrus.Debugf("Error enabling automerge: %s", err.Error())
			}
			return err
		}
	}

	return nil
}

// closePullRequest closes an existing Pull Request using GitHub graphql api.
func (p *PullRequest) closePullRequest() error {

	// https://docs.github.com/en/graphql/reference/input-objects#closepullrequestinput
	/*
		  mutation($input: closePullRequestInput!){
			closePullRequest(input:$input){
			  pullRequest{
				url
			    title
			    body
			  }
			}
		  }

		  {
			"input": {
			  "pullRequestId" : "yyy"
			}
		  }
	*/
	var mutation struct {
		UpdatePullRequest struct {
			PullRequest PullRequestApi
		} `graphql:"closePullRequest(input: $input)"`
	}

	input := githubv4.ClosePullRequestInput{
		PullRequestID: githubv4.ID(p.remotePullRequest.ID),
	}

	err := p.gh.client.Mutate(context.Background(), &mutation, input, nil)
	if err != nil {
		logrus.Debugf("Closing pull request: %s", err.Error())
		return err
	}

	msg := "Pull request closed as no changed file detected"
	logrus.Infof("%s at:\n\n\t%s\n\n", msg, mutation.UpdatePullRequest.PullRequest.Url)
	err = p.addComment(msg)
	if err != nil {
		logrus.Errorf("Commenting pull-request: %s", err.Error())
	}

	return nil
}

func (p *PullRequest) isAhead() bool {
	return p.repository.Status == "AHEAD"
}

// updatePullRequest updates an existing Pull Request.
func (p *PullRequest) updatePullRequest() error {

	/*
		  mutation($input: UpdatePullRequestInput!){
			updatePullRequest(input:$input){
			  pullRequest{
				url
			    title
			    body
			  }
			}
		  }

		  {
			"input": {
			  "title":"xxx",
			  "pullRequestId" : "yyy"
			}
		  }
	*/
	var mutation struct {
		UpdatePullRequest struct {
			PullRequest PullRequestApi
		} `graphql:"updatePullRequest(input: $input)"`
	}

	logrus.Debugf("Updating GitHub Pull Request")

	title := p.Title

	bodyPR, err := utils.GeneratePullRequestBody(p.spec.Description, p.Report)
	if err != nil {
		return err
	}

	labelsID := []githubv4.ID{}
	repositoryLabels, err := p.gh.getRepositoryLabels()
	if err != nil {
		logrus.Debugf("Error fetching repository labels: %s", err.Error())
		return err
	}

	matchingLabels := []repositoryLabelApi{}
	for _, l := range p.spec.Labels {
		for _, repoLabel := range repositoryLabels {
			if l == repoLabel.Name {
				matchingLabels = append(matchingLabels, repoLabel)
			}
		}
	}

	remotePRLabels, err := p.GetPullRequestLabelsInformation()
	if err != nil {
		logrus.Debugf("Error fetching labels information: %s", err.Error())
		return err
	}

	// Build a list of labelID to update the pullrequest
	for _, label := range mergeLabels(matchingLabels, remotePRLabels) {
		labelsID = append(labelsID, githubv4.NewID(label.ID))
	}

	input := githubv4.UpdatePullRequestInput{
		PullRequestID: githubv4.ID(p.remotePullRequest.ID),
		Title:         githubv4.NewString(githubv4.String(title)),
		Body:          githubv4.NewString(githubv4.String(bodyPR)),
	}

	if len(p.spec.Labels) != 0 {
		input.LabelIDs = &labelsID
	}

	err = p.gh.client.Mutate(context.Background(), &mutation, input, nil)
	if err != nil {
		logrus.Debugf("Error updating pull-request: %s", err.Error())
		return err
	}

	logrus.Infof("\nPull Request available at:\n\n\t%s\n\n", mutation.UpdatePullRequest.PullRequest.Url)

	return nil
}

// EnablePullRequestAutoMerge updates an existing pullrequest with the flag automerge
func (p *PullRequest) EnablePullRequestAutoMerge() error {

	// Test that automerge feature is enabled on repository but only if we plan to use it
	autoMergeAllowed, err := p.isAutoMergedEnabledOnRepository()
	if err != nil {
		return err
	}

	if !autoMergeAllowed {
		return ErrAutomergeNotAllowOnRepository
	}

	input := githubv4.EnablePullRequestAutoMergeInput{
		PullRequestID: githubv4.String(p.remotePullRequest.ID),
	}

	// The GitHub Api expects the merge method to be capital letter and don't allows empty value
	// hence the reason to set input.MergeMethod only if the value is not nil
	if len(p.spec.MergeMethod) > 0 {
		mergeMethod := githubv4.PullRequestMergeMethod(strings.ToUpper(p.spec.MergeMethod))
		input.MergeMethod = &mergeMethod
	}

	if p.spec.UseTitleForAutoMerge {
		if strings.EqualFold(p.spec.MergeMethod, "squash") {
			input.CommitHeadline = githubv4.NewString(githubv4.String(fmt.Sprintf("%s (#%d)", p.Title, p.remotePullRequest.Number)))
		} else if strings.EqualFold(p.spec.MergeMethod, "rebase") {
			input.CommitHeadline = githubv4.NewString(githubv4.String(p.Title))
		}
	}

	var mutation mutationEnablePullRequestAutoMerge
	err = p.gh.client.Mutate(context.Background(), &mutation, input, nil)

	if err != nil {
		return err
	}

	return nil
}

// OpenPullRequest creates a new GitHub Pull Request.
func (p *PullRequest) OpenPullRequest() error {

	/*
	   mutation($input: CreatePullRequestInput!){
	     createPullRequest(input:$input){
	       pullRequest{
	         url
	       }
	     }
	   }

	   {
	     "input":{
	       "baseRefName": "x" ,
	       "repositoryId":"y",
	       "headRefName": "z",
	       "headRepositoryId": "",
	       "title",
	       "body",
	     }
	   }


	*/
	bodyPR, err := utils.GeneratePullRequestBody(p.spec.Description, p.Report)
	if err != nil {
		return err
	}

	_, workingBranch, targetBranch := p.gh.GetBranches()

	isAhead := p.isAhead()

	if !isAhead {
		logrus.Debugf("GitHub pullrequest not needed")
		return nil
	}

	input := githubv4.CreatePullRequestInput{
		RepositoryID:        githubv4.String(p.repository.ID),
		BaseRefName:         githubv4.String(targetBranch),
		HeadRefName:         githubv4.String(workingBranch),
		Title:               githubv4.String(p.Title),
		Body:                githubv4.NewString(githubv4.String(bodyPR)),
		MaintainerCanModify: githubv4.NewBoolean(githubv4.Boolean(!p.spec.MaintainerCannotModify)),
		Draft:               githubv4.NewBoolean(githubv4.Boolean(p.spec.Draft)),
	}

	if p.spec.Parent {
		input.RepositoryID = githubv4.String(p.repository.ParentID)
		input.HeadRepositoryID = githubv4.NewID(p.repository.ID)
	}

	logrus.Debugf("Opening pull-request against repo from %q to %q", input.BaseRefName, input.HeadRefName)

	var mutation struct {
		CreatePullRequest struct {
			PullRequest PullRequestApi
		} `graphql:"createPullRequest(input: $input)"`
	}

	err = p.gh.client.Mutate(context.Background(), &mutation, input, nil)
	if err != nil {
		logrus.Infof("\nError creating pull request:\n\n\t%s\n\n", err.Error())
		return err
	}

	p.remotePullRequest = mutation.CreatePullRequest.PullRequest

	return nil

}

// isAutoMergedEnabledOnRepository checks if a remote repository allows automerging Pull Requests.
func (p *PullRequest) isAutoMergedEnabledOnRepository() (bool, error) {

	var query struct {
		Repository struct {
			AutoMergeAllowed bool
		} `graphql:"repository(owner: $owner, name: $name)"`
	}

	variables := map[string]interface{}{
		"owner": githubv4.String(p.gh.Spec.Owner),
		"name":  githubv4.String(p.gh.Spec.Repository),
	}

	err := p.gh.client.Query(context.Background(), &query, variables)

	if err != nil {
		return false, err
	}
	return query.Repository.AutoMergeAllowed, nil

}

// getRemotePullRequest checks if a Pull Request already exists on GitHub and is in the state 'open' or 'closed'.
func (p *PullRequest) getRemotePullRequest(resetBody bool) error {
	/*
		https://developer.github.com/v4/explorer/
		# Query
		query getPullRequests(
			$owner: String!,
			$name:String!,
			$baseRefName:String!,
			$headRefName:String!){
				repository(owner: $owner, name: $name) {
					pullRequests(baseRefName: $baseRefName, headRefName: $headRefName, last: 1) {
						nodes {
							state
							id
						}
					}
				}
			}
		}
		# Variables
		{
			"owner": "olblak",
			"name": "charts",
			"baseRefName": "master",
			"headRefName": "updatecli/HelmChart/2.4.0"
		}
	*/

	var query struct {
		Repository struct {
			PullRequests struct {
				Nodes []PullRequestApi
			} `graphql:"pullRequests(baseRefName: $baseRefName, headRefName: $headRefName, last: 1, states: [OPEN])"`
		} `graphql:"repository(owner: $owner, name: $name)"`
	}

	owner := githubv4.String(p.repository.Owner)
	name := githubv4.String(p.repository.Name)

	if p.spec.Parent {
		owner = githubv4.String(p.repository.ParentOwner)
		name = githubv4.String(p.repository.ParentName)
	}

	_, workingBranch, targetBranch := p.gh.GetBranches()

	variables := map[string]interface{}{
		"owner":       owner,
		"name":        name,
		"baseRefName": githubv4.String(targetBranch),
		"headRefName": githubv4.String(workingBranch),
	}

	err := p.gh.client.Query(context.Background(), &query, variables)
	if err != nil {
		logrus.Debugf("Error getting existing pull-request: %s", err.Error())
		return err
	}

	// If no pull-request found, then we can exit
	if len(query.Repository.PullRequests.Nodes) == 0 {
		logrus.Debugf("No existing pull-request found in repo: %s/%s", owner, name)
		return nil
	}

	p.remotePullRequest = query.Repository.PullRequests.Nodes[0]
	// If a remote pullrequest already exist, then we reuse its body to generate the final one
	switch resetBody {
	case false:
		logrus.Debugf("Merging existing pull-request body with new report")
		p.Report = reports.MergeFromString(p.remotePullRequest.Body, p.Report)
	case true:
		logrus.Debugf("Resetting pull-request body with new report")
	}

	logrus.Infof("Existing GitHub pull request found: %s", p.remotePullRequest.Url)

	return nil
}

// getPullRequestLabelsInformation queries GitHub Api to retrieve every labels assigned to a pullRequest
func (p *PullRequest) GetPullRequestLabelsInformation() ([]repositoryLabelApi, error) {

	/*
		query getPullRequests(
		  $owner: String!,
		  $name:String!,
		  $before:Int!){
			repository(owner: $owner, name: $name) {
			  pullRequest(number: 4){
				labels(last: 5, before:$before){
				  totalCount
				  pageInfo {
					hasNextPage
					endCursor
				  }
				  edges {
					node {
					  id
					  name
					  description
					}
					cursor
				  }
				}
			  }
			}
		  }
	*/

	owner := githubv4.String(p.repository.Owner)
	repo := githubv4.String(p.repository.Name)

	if p.spec.Parent {
		owner = githubv4.String(p.repository.ParentOwner)
		repo = githubv4.String(p.repository.ParentName)
	}

	variables := map[string]interface{}{
		"owner":      owner,
		"repository": repo,
		"number":     githubv4.Int(p.remotePullRequest.Number),
		"before":     (*githubv4.String)(nil),
	}

	var query struct {
		RateLimit  RateLimit
		Repository struct {
			PullRequest struct {
				Labels struct {
					TotalCount int
					PageInfo   PageInfo
					Edges      []struct {
						Cursor string
						Node   struct {
							ID          string
							Name        string
							Description string
						}
					}
				} `graphql:"labels(last: 5, before: $before)"`
			} `graphql:"pullRequest(number: $number)"`
		} `graphql:"repository(owner: $owner, name: $repository)"`
	}

	var pullRequestLabels []repositoryLabelApi
	for {
		err := p.gh.client.Query(context.Background(), &query, variables)

		if err != nil {
			logrus.Errorf("\t%s", err)
			return nil, err
		}

		query.RateLimit.Show()

		// Retrieve remote label information such as label ID, label name, labe description
		for _, node := range query.Repository.PullRequest.Labels.Edges {
			pullRequestLabels = append(
				pullRequestLabels,
				repositoryLabelApi{
					ID:          node.Node.ID,
					Name:        node.Node.Name,
					Description: node.Node.Description,
				})
		}

		if !query.Repository.PullRequest.Labels.PageInfo.HasPreviousPage {
			break
		}

		variables["before"] = githubv4.NewString(githubv4.String(query.Repository.PullRequest.Labels.PageInfo.StartCursor))
	}
	return pullRequestLabels, nil
}
