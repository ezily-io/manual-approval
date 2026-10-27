package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v43/github"
)

// timeNow is a seam for tests to pin the clock; production code should
// always call this instead of time.Now directly.
var timeNow = time.Now

type approvalEnvironment struct {
	client              *github.Client
	repoFullName        string
	repo                string
	repoOwner           string
	runID               int
	workflowName        string
	commitSHA           string
	approvalIssue       *github.Issue
	approvalIssueNumber int
	issueTitle          string
	issueBody           string
	issueApprovers      []string
	minimumApprovals    int
	targetRepoOwner     string
	targetRepoName      string
	failOnDenial        bool
}

func newApprovalEnvironment(client *github.Client, repoFullName, repoOwner string, runID int, approvers []string, minimumApprovals int, issueTitle, issueBody string, targetRepoOwner string, targetRepoName string, failOnDenial bool) (*approvalEnvironment, error) {
	repoOwnerAndName := strings.Split(repoFullName, "/")
	if len(repoOwnerAndName) != 2 {
		return nil, fmt.Errorf("repo owner and name in unexpected format: %s", repoFullName)
	}
	repo := repoOwnerAndName[1]

	return &approvalEnvironment{
		client:           client,
		repoFullName:     repoFullName,
		repo:             repo,
		repoOwner:        repoOwner,
		runID:            runID,
		issueApprovers:   approvers,
		minimumApprovals: minimumApprovals,
		issueTitle:       issueTitle,
		issueBody:        issueBody,
		targetRepoOwner:  targetRepoOwner,
		targetRepoName:   targetRepoName,
		failOnDenial:     failOnDenial,
	}, nil
}

func (a approvalEnvironment) runURL() string {
	baseUrl := a.client.BaseURL.String()
	if strings.Contains(baseUrl, "github.com") {
		baseUrl = "https://github.com/"
	}
	return fmt.Sprintf("%s%s/actions/runs/%d", baseUrl, a.repoFullName, a.runID)
}

// resolvedIssueTitle returns the caller-supplied issue title, or a title
// derived from the run ID if none was given. The latter is always unique per
// run, so it never matches an existing issue in findExistingIssue - this
// keeps find-or-create a no-op (always create) for callers who don't pass a
// stable issue-title, matching the original behavior.
func (a *approvalEnvironment) resolvedIssueTitle() string {
	if a.issueTitle != "" {
		return a.issueTitle
	}
	return fmt.Sprintf("Manual approval required for workflow run %d", a.runID)
}

// findOrCreateApprovalIssue reuses an existing open or closed issue whose
// title exactly matches the resolved issue title - for example one already
// created by another workflow run approving the same deployment batch - or
// creates a new one if none exists yet.
func (a *approvalEnvironment) findOrCreateApprovalIssue(ctx context.Context) error {
	found, err := a.findExistingIssue(ctx)
	if err != nil {
		return err
	}
	if found {
		fmt.Printf("Reusing existing issue: %s\n", a.approvalIssue.GetHTMLURL())
		return nil
	}

	return a.createApprovalIssue(ctx)
}

func (a *approvalEnvironment) findExistingIssue(ctx context.Context) (bool, error) {
	title := a.resolvedIssueTitle()
	matches, err := a.searchIssuesByTitle(ctx, title)
	if err != nil {
		return false, fmt.Errorf("failed to search for existing approval issue: %w", err)
	}
	if len(matches) == 0 {
		return false, nil
	}

	canonical := lowestNumberedIssue(matches)
	a.approvalIssue = canonical
	a.approvalIssueNumber = canonical.GetNumber()
	if err := a.registerAsListener(ctx); err != nil {
		fmt.Printf("warning: failed to register as a listener on issue #%d: %v\n", a.approvalIssueNumber, err)
	}
	return true, nil
}

// registerAsListener records this workflow run in the issue body's
// "Listening workflows" list, so anyone reading the issue can see every
// workflow run currently waiting on it - not just the one that created it.
// A no-op if this run is already listed (e.g. a retried step) or if the
// workflow name isn't available. Re-fetches the issue immediately before
// editing to narrow (not eliminate) the lost-update race against other
// runs registering at the same time.
func (a *approvalEnvironment) registerAsListener(ctx context.Context) error {
	if a.workflowName == "" {
		return nil
	}

	issue, _, err := a.client.Issues.Get(ctx, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber)
	if err != nil {
		return fmt.Errorf("failed to fetch issue #%d: %w", a.approvalIssueNumber, err)
	}

	entry := listenerEntry(a.workflowName, a.runURL(), a.commitSHA, timeNow())
	body := issue.GetBody()
	if strings.Contains(body, entry) {
		return nil
	}

	newBody := body + "\n" + entry
	updated, _, err := a.client.Issues.Edit(ctx, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber, &github.IssueRequest{Body: &newBody})
	if err != nil {
		return fmt.Errorf("failed to update issue body: %w", err)
	}
	a.approvalIssue = updated
	return nil
}

func listenerEntry(workflowName, runURL, commitSHA string, at time.Time) string {
	return fmt.Sprintf("- [%s](%s) — `%s` at %s", workflowName, runURL, shortSHA(commitSHA), at.UTC().Format("2006-01-02 15:04:05 MST"))
}

func shortSHA(sha string) string {
	const shortLength = 7
	if len(sha) > shortLength {
		return sha[:shortLength]
	}
	return sha
}

// formatListenerSection renders the initial "Listening workflows" section
// for a newly created issue, naming the creating run as the first listener.
// Later runs that find and reuse this issue append their own entry to this
// same section via registerAsListener.
func formatListenerSection(workflowName, runURL, commitSHA string, at time.Time) string {
	if workflowName == "" {
		return ""
	}
	return fmt.Sprintf("\n\n**Listening workflows:**\n%s", listenerEntry(workflowName, runURL, commitSHA, at))
}

// reconcileDuplicate guards against the race where two workflow runs each
// search for an existing issue at nearly the same time, both find nothing
// (GitHub's search index lags slightly behind writes), and both create one.
// It's called on every iteration of the comment-polling loop rather than
// once after creation: search index lag is variable and sometimes exceeds
// a few seconds, so a single delayed check isn't reliable, but the loop
// already runs every pollingInterval for the duration of the wait, so
// piggybacking this check costs nothing extra in wall-clock time. If more
// than one issue now matches, defers to the lowest-numbered one (the first
// created), closing this run's issue as a duplicate.
func (a *approvalEnvironment) reconcileDuplicate(ctx context.Context) error {
	title := a.resolvedIssueTitle()
	matches, err := a.searchIssuesByTitle(ctx, title)
	if err != nil {
		// Best-effort check; a transient search failure shouldn't fail the run.
		fmt.Printf("warning: failed to check for duplicate approval issues: %v\n", err)
		return nil
	}
	if len(matches) <= 1 {
		return nil
	}

	canonical := lowestNumberedIssue(matches)
	if canonical.GetNumber() == a.approvalIssueNumber {
		return nil
	}

	fmt.Printf("Detected a duplicate approval issue (#%d); deferring to #%d\n", a.approvalIssueNumber, canonical.GetNumber())

	duplicateComment := fmt.Sprintf("Duplicate approval issue; deferring to #%d.", canonical.GetNumber())
	if _, _, err := a.client.Issues.CreateComment(ctx, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber, &github.IssueComment{Body: &duplicateComment}); err != nil {
		return fmt.Errorf("failed to comment on duplicate issue: %w", err)
	}

	closedState := "closed"
	if _, _, err := a.client.Issues.Edit(ctx, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber, &github.IssueRequest{State: &closedState}); err != nil {
		return fmt.Errorf("failed to close duplicate issue: %w", err)
	}

	a.approvalIssue = canonical
	a.approvalIssueNumber = canonical.GetNumber()
	if err := a.registerAsListener(ctx); err != nil {
		fmt.Printf("warning: failed to register as a listener on issue #%d: %v\n", a.approvalIssueNumber, err)
	}
	return nil
}

func (a *approvalEnvironment) searchIssuesByTitle(ctx context.Context, title string) ([]*github.Issue, error) {
	query := fmt.Sprintf(`repo:%s/%s is:issue in:title "%s"`, a.targetRepoOwner, a.targetRepoName, title)
	result, _, err := a.client.Search.Issues(ctx, query, nil)
	if err != nil {
		return nil, err
	}
	// The search API matches on tokens within the title, so defensively
	// require an exact title match rather than trusting it did a phrase match.
	return exactTitleMatches(result.Issues, title), nil
}

func exactTitleMatches(issues []*github.Issue, title string) []*github.Issue {
	matches := make([]*github.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.GetTitle() == title {
			matches = append(matches, issue)
		}
	}
	return matches
}

// lowestNumberedIssue returns the issue with the smallest number, i.e. the
// one created first, used as the canonical issue when more than one match.
func lowestNumberedIssue(issues []*github.Issue) *github.Issue {
	canonical := issues[0]
	for _, issue := range issues[1:] {
		if issue.GetNumber() < canonical.GetNumber() {
			canonical = issue
		}
	}
	return canonical
}

func formatCustomIssueBody(body string) string {
	if body == "" {
		return ""
	}
	return fmt.Sprintf("\n%s\n", body)
}

func (a *approvalEnvironment) createApprovalIssue(ctx context.Context) error {
	issueTitle := a.resolvedIssueTitle()

	approversBody := ""
	for _, approver := range a.issueApprovers {
		approversBody = fmt.Sprintf("%s> * @%s\n", approversBody, approver)
	}

	// Embed the caller-supplied issue-body directly in the issue so it's
	// visible up front, unless it's long enough to risk exceeding GitHub's
	// issue body size limit - then it's posted as a follow-up comment instead.
	customBody := a.issueBody
	deferCustomBodyToComment := len(customBody) > maxEmbeddedCustomBodyLength
	if deferCustomBodyToComment {
		customBody = ""
	}

	issueBody := fmt.Sprintf(`> Workflow is pending manual review.
> URL: %s
%s
> [!IMPORTANT]
> Required approvers:
%s

> [!TIP]
> Respond %s to continue workflow or %s to cancel.%s`,
		a.runURL(),
		formatCustomIssueBody(customBody),
		approversBody,
		formatAcceptedWords(approvedWords),
		formatAcceptedWords(deniedWords),
		formatListenerSection(a.workflowName, a.runURL(), a.commitSHA, timeNow()),
	)

	issueBody = fmt.Sprintf(">[!NOTE]\n%s", issueBody)

	var err error
	fmt.Printf(
		"Creating issue in repo %s/%s with the following content:\nTitle: %s\nApprovers: %s\nBody:\n%s\n",
		a.targetRepoOwner,
		a.targetRepoName,
		issueTitle,
		a.issueApprovers,
		issueBody,
	)
	a.approvalIssue, _, err = a.client.Issues.Create(ctx, a.targetRepoOwner, a.targetRepoName, &github.IssueRequest{
		Title:     &issueTitle,
		Body:      &issueBody,
		Assignees: &a.issueApprovers,
	})
	if err != nil {
		return err
	}
	a.approvalIssueNumber = a.approvalIssue.GetNumber()

	if deferCustomBodyToComment && a.issueBody != "" {
		bodyChunks := splitLongString(a.issueBody)
		for _, chunk := range bodyChunks {
			_, _, err = a.client.Issues.CreateComment(ctx, a.targetRepoOwner, a.targetRepoName, *a.approvalIssue.Number, &github.IssueComment{
				Body: &chunk,
			})
			if err != nil {
				return fmt.Errorf("failed to add comment chunk to issue: %w", err)
			}
		}
	}

	fmt.Printf("Issue created: %s\n", a.approvalIssue.GetHTMLURL())
	return nil
}

func (a *approvalEnvironment) SetActionOutputs(outputs map[string]string) (bool, error) {
	outputFile := os.Getenv("GITHUB_OUTPUT")
	if outputFile == "" {
		return false, nil
	}

	f, err := os.OpenFile(outputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}

    defer func() {
        _ = f.Close() // Error explicitly ignored as there is nothing to handle if file close fails.
    }()

	var pairs []string

	for key, value := range outputs {
		pairs = append(pairs, fmt.Sprintf("%s=%s", key, value))
	}

	// Add a newline before writing the new outputs if the file is not empty. This prevents
	// two outputs from being written on the same line.
	fileInfo, err := f.Stat()
	if err != nil {
		return false, err
	}
	if fileInfo.Size() > 0 {
		if _, err := f.WriteString("\n"); err != nil {
			return false, err
		}
	}

	if _, err := f.WriteString(strings.Join(pairs, "\n")); err != nil {
		return false, err
	}

	return true, nil
}

func approvalFromComments(comments []*github.IssueComment, approvers []string, minimumApprovals int) (approvalStatus, error) {
	remainingApprovers := make([]string, len(approvers))
	copy(remainingApprovers, approvers)

	if minimumApprovals == 0 {
		minimumApprovals = len(approvers)
	}

	for _, comment := range comments {
		commentUser := comment.User.GetLogin()
		approverIdx := approversIndex(remainingApprovers, commentUser)
		if approverIdx < 0 {
			continue
		}

		commentBody := comment.GetBody()
		isApprovalComment, err := isApproved(commentBody)
		if err != nil {
			return approvalStatusPending, err
		}
		if isApprovalComment {
			if len(remainingApprovers) == len(approvers)-minimumApprovals+1 {
				return approvalStatusApproved, nil
			}
			remainingApprovers[approverIdx] = remainingApprovers[len(remainingApprovers)-1]
			remainingApprovers = remainingApprovers[:len(remainingApprovers)-1]
			continue
		}

		isDenialComment, err := isDenied(commentBody)
		if err != nil {
			return approvalStatusPending, err
		}
		if isDenialComment {
			return approvalStatusDenied, nil
		}
	}

	return approvalStatusPending, nil
}

func approversIndex(approvers []string, name string) int {
	for idx, approver := range approvers {
		if strings.EqualFold(approver, name) {
			return idx
		}
	}
	return -1
}

func isApproved(commentBody string) (bool, error) {
	for _, approvedWord := range approvedWords {
		re, err := regexp.Compile(fmt.Sprintf("(?i)^%s[.!]*\n*\\s*$", approvedWord))
		if err != nil {
			fmt.Printf("Error parsing. %v", err)
			return false, err
		}

		matched := re.MatchString(commentBody)

		if matched {
			return true, nil
		}
	}

	return false, nil
}

func isDenied(commentBody string) (bool, error) {
	for _, deniedWord := range deniedWords {
		re, err := regexp.Compile(fmt.Sprintf("(?i)^%s[.!]*\n*\\s*$", deniedWord))
		if err != nil {
			fmt.Printf("Error parsing. %v", err)
			return false, err
		}
		matched := re.MatchString(commentBody)
		if matched {
			return true, nil
		}
	}

	return false, nil
}

func formatAcceptedWords(words []string) string {
	var quotedWords []string

	for _, word := range words {
		quotedWords = append(quotedWords, fmt.Sprintf("\"%s\"", word))
	}

	return strings.Join(quotedWords, ", ")
}

func splitLongLine(line string, maxL int) ([]string, bool) {
	if len(line) <= maxL {
		return []string{line}, false
	}

	words := strings.Fields(line)
	var result []string
	var currentLine string

	for _, word := range words {
		if len(currentLine)+len(word)+1 > maxL {
			result = append(result, currentLine)
			currentLine = word
		} else {
			if currentLine != "" {
				currentLine += " "
			}
			currentLine += word
		}
	}
	if currentLine != "" {
		result = append(result, currentLine)
	}
	return result, true
}

func splitLongString(input string) []string {
	maxLength := 65536
	var result []string

	lines := strings.Split(input, "\n")
	currentChunk := strings.Builder{}
	currentLength := 0

	for i, line := range lines {
    lineLength := len(line)
		if i < len(lines)-1 {
			lineLength++
    }

		if currentLength+lineLength > maxLength {
			if currentChunk.Len() > 0 {
				result = append(result, currentChunk.String())
				currentChunk.Reset()
				currentLength = 0
			}
		}

		lineSplit, isLongLine := splitLongLine(line, maxLength)
		if isLongLine {
			if currentChunk.Len() > 0 {
				result = append(result, currentChunk.String())
				currentChunk.Reset()
			}
			result = append(result, lineSplit[:len(lineSplit)-1]...)
			currentChunk.WriteString(lineSplit[len(lineSplit)-1])
			currentLength = len(lineSplit[len(lineSplit)-1])
		} else {
			currentChunk.WriteString(line)
			currentLength += lineLength
		}

		if i < len(lines)-1 {
			currentChunk.WriteString("\n")
		}
	}
	if currentChunk.Len() > 0 {
		result = append(result, currentChunk.String())
	}
	return result
}

