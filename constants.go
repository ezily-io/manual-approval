package main

import (
	"os"
	"strings"
	"time"
)

const (
	pollingInterval time.Duration = 10 * time.Second

	// maxEmbeddedCustomBodyLength keeps the caller-supplied issue-body embedded
	// in the issue's own body safely under GitHub's issue body size limit. If
	// it's longer than this, it's posted as a follow-up comment instead.
	maxEmbeddedCustomBodyLength int = 60000

	envVarRepoFullName                       string = "GITHUB_REPOSITORY"
	envVarRunID                              string = "GITHUB_RUN_ID"
	envVarRepoOwner                          string = "GITHUB_REPOSITORY_OWNER"
	envVarWorkflowInitiator                  string = "GITHUB_ACTOR"
	envVarToken                              string = "INPUT_SECRET"
	envVarApprovers                          string = "INPUT_APPROVERS"
	envVarMinimumApprovals                   string = "INPUT_MINIMUM-APPROVALS"
	envVarIssueTitle                         string = "INPUT_ISSUE-TITLE"
	envVarIssueBody                          string = "INPUT_ISSUE-BODY"
	envVarIssueBodyFilePath                  string = "INPUT_ISSUE-BODY-FILE-PATH"
	envVarExcludeWorkflowInitiatorAsApprover string = "INPUT_EXCLUDE-WORKFLOW-INITIATOR-AS-APPROVER"
	envVarAdditionalApprovedWords            string = "INPUT_ADDITIONAL-APPROVED-WORDS"
	envVarAdditionalDeniedWords              string = "INPUT_ADDITIONAL-DENIED-WORDS"
	envVarFailOnDenial                       string = "INPUT_FAIL-ON-DENIAL"
	envVarTargetRepoOwner                    string = "INPUT_TARGET-REPOSITORY-OWNER"
	envVarTargetRepo                         string = "INPUT_TARGET-REPOSITORY"
)

var (
	additionalApprovedWords = readAdditionalWords(envVarAdditionalApprovedWords)
	additionalDeniedWords   = readAdditionalWords(envVarAdditionalDeniedWords)

	approvedWords = append([]string{"approved", "approve", "lgtm", "yes"}, additionalApprovedWords...)
	deniedWords   = append([]string{"denied", "deny", "no"}, additionalDeniedWords...)

	// duplicateReconcileDelay gives GitHub's search index a moment to catch up
	// before re-checking for a duplicate issue created by a concurrent workflow
	// run. It's a var (not const) so tests can shrink it to 0.
	duplicateReconcileDelay = 5 * time.Second
)

func readAdditionalWords(envVar string) []string {
	rawValue := strings.TrimSpace(os.Getenv(envVar))
	if len(rawValue) == 0 {
		// Nothing else to do here.
		return []string{}
	}
	slicedWords := strings.Split(rawValue, ",")
	for i := range slicedWords {
		// no leading or trailing spaces in user provided words.
		slicedWords[i] = strings.TrimSpace(slicedWords[i])
	}
	return slicedWords
}
