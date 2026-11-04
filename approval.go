package main

import (
	"context"
	"errors"
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
	client                *github.Client
	repoFullName          string
	repo                  string
	repoOwner             string
	runID                 int
	workflowName          string
	commitSHA             string
	approvalIssue         *github.Issue
	approvalIssueNumber   int
	supersededIssueNumber int
	issueTitle            string
	issueBody             string
	issueApprovers        []string
	minimumApprovals      int
	targetRepoOwner       string
	targetRepoName        string
	failOnDenial          bool
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

// findOrCreateApprovalIssue reuses an existing open issue, or a closed one
// with a clear approve/deny outcome, whose title exactly matches the
// resolved issue title - for example one already created by another
// workflow run approving the same deployment batch. If every match has been
// abandoned (closed with no resolution - e.g. a run that timed out or was
// cancelled before anyone responded), it creates a new issue and links the
// two together, rather than reusing a dead one.
func (a *approvalEnvironment) findOrCreateApprovalIssue(ctx context.Context) error {
	found, err := a.findExistingIssue(ctx)
	if err != nil {
		return err
	}
	if found {
		fmt.Printf("Reusing existing issue: %s\n", a.approvalIssue.GetHTMLURL())
		return nil
	}

	if err := a.createApprovalIssue(ctx); err != nil {
		return err
	}

	if a.supersededIssueNumber != 0 {
		if err := a.linkSupersededIssue(ctx); err != nil {
			fmt.Printf("warning: failed to link superseded issue #%d: %v\n", a.supersededIssueNumber, err)
		}
	}
	return nil
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

	canonical, usable, err := a.canonicalIssue(ctx, matches)
	if err != nil {
		return false, err
	}
	if !usable {
		// Every match has been abandoned - nothing to reuse. Remember which
		// one, so createApprovalIssue can link the new issue back to it and
		// note on the old one that it's been superseded.
		a.supersededIssueNumber = canonical.GetNumber()
		return false, nil
	}

	a.approvalIssue = canonical
	a.approvalIssueNumber = canonical.GetNumber()
	if err := a.registerAsListener(ctx); err != nil {
		fmt.Printf("warning: failed to register as a listener on issue #%d: %v\n", a.approvalIssueNumber, err)
	}
	return true, nil
}

// canonicalIssue picks the lowest-numbered issue among matches that's either
// still open, or closed with a clear approve/deny outcome in its comments.
// If every match has been abandoned (closed with no resolution), it still
// returns the lowest-numbered one as a reference point, with usable=false so
// the caller knows to supersede it rather than reuse or defer to it.
func (a *approvalEnvironment) canonicalIssue(ctx context.Context, issues []*github.Issue) (issue *github.Issue, usable bool, err error) {
	var best *github.Issue
	for _, candidate := range issues {
		ok, err := a.isUsable(ctx, candidate)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		if best == nil || candidate.GetNumber() < best.GetNumber() {
			best = candidate
		}
	}
	if best != nil {
		return best, true, nil
	}
	// Every match is abandoned. Reference the most recent one (highest
	// number), not the original, so repeated abandon-and-retry chains link
	// consecutively (old -> retry1 -> retry2) instead of every retry fanning
	// out from the very first attempt.
	return highestNumberedIssue(issues), false, nil
}

// isUsable reports whether an issue is still open, or was closed with a
// clear approve/deny outcome - as opposed to abandoned (closed with no
// resolution, e.g. a run that timed out or was cancelled before anyone
// responded).
func (a *approvalEnvironment) isUsable(ctx context.Context, issue *github.Issue) (bool, error) {
	if issue.GetState() != "closed" {
		return true, nil
	}
	comments, _, err := a.client.Issues.ListComments(ctx, a.targetRepoOwner, a.targetRepoName, issue.GetNumber(), &github.IssueListCommentsOptions{})
	if err != nil {
		return false, fmt.Errorf("failed to list comments on issue #%d: %w", issue.GetNumber(), err)
	}
	status, err := approvalFromComments(comments, a.issueApprovers, a.minimumApprovals)
	if err != nil {
		return false, fmt.Errorf("failed to evaluate comments on issue #%d: %w", issue.GetNumber(), err)
	}
	return status == approvalStatusApproved || status == approvalStatusDenied, nil
}

func (a *approvalEnvironment) linkSupersededIssue(ctx context.Context) error {
	comment := fmt.Sprintf("Superseded by #%d.", a.approvalIssueNumber)
	if _, _, err := a.client.Issues.CreateComment(ctx, a.targetRepoOwner, a.targetRepoName, a.supersededIssueNumber, &github.IssueComment{Body: &comment}); err != nil {
		return fmt.Errorf("failed to comment on superseded issue #%d: %w", a.supersededIssueNumber, err)
	}
	return nil
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

// reconcileDuplicate keeps this run pointed at the best available issue. It
// handles two situations, both found the same way - searching by title again
// on every iteration of the comment-polling loop, since it's already running
// every pollingInterval for the duration of the wait:
//  1. Two workflow runs raced to create an issue at nearly the same time
//     (GitHub's search index lags slightly behind writes, so both found
//     nothing and both created one). The higher-numbered one defers to the
//     lower-numbered (first created).
//  2. The issue this run is tracking gets abandoned mid-wait - e.g. another
//     listener on a shared issue was cancelled, closing it for everyone -
//     and a different run has since superseded it with a new one. This run
//     switches to that replacement instead of waiting on a dead issue.
//
// Either way, this run's current issue is closed (if not already) and this
// run re-registers itself as a listener on whichever issue is canonical now.
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

	canonical, usable, err := a.canonicalIssue(ctx, matches)
	if err != nil {
		fmt.Printf("warning: failed to evaluate duplicate approval issues: %v\n", err)
		return nil
	}
	if !usable || canonical.GetNumber() == a.approvalIssueNumber {
		return nil
	}

	fmt.Printf("Found a better approval issue (#%d) than the one this run is tracking (#%d); switching\n", canonical.GetNumber(), a.approvalIssueNumber)

	deferComment := fmt.Sprintf("Deferring to #%d.", canonical.GetNumber())
	if _, _, err := a.client.Issues.CreateComment(ctx, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber, &github.IssueComment{Body: &deferComment}); err != nil {
		return fmt.Errorf("failed to comment on issue #%d: %w", a.approvalIssueNumber, err)
	}
	if err := closeIssueIdempotent(ctx, a.client, a.targetRepoOwner, a.targetRepoName, a.approvalIssueNumber); err != nil {
		return fmt.Errorf("failed to close issue #%d: %w", a.approvalIssueNumber, err)
	}

	a.approvalIssue = canonical
	a.approvalIssueNumber = canonical.GetNumber()
	if err := a.registerAsListener(ctx); err != nil {
		fmt.Printf("warning: failed to register as a listener on issue #%d: %v\n", a.approvalIssueNumber, err)
	}
	return nil
}

// searchIssuesByTitle searches for issues matching title, retrying with
// backoff when GitHub's Search API rate limit is hit. Many workflow runs can
// call this at roughly the same time (they're all triggered by the same
// push), which can exceed the Search API's 30-requests-per-minute limit even
// though each individual run's own call volume is well within it.
func (a *approvalEnvironment) searchIssuesByTitle(ctx context.Context, title string) ([]*github.Issue, error) {
	query := fmt.Sprintf(`repo:%s/%s is:issue in:title "%s"`, a.targetRepoOwner, a.targetRepoName, title)

	var lastErr error
	for attempt := 0; attempt <= maxSearchRetries; attempt++ {
		result, _, err := a.client.Search.Issues(ctx, query, nil)
		if err == nil {
			// The search API matches on tokens within the title, so
			// defensively require an exact title match rather than trusting
			// it did a phrase match.
			return exactTitleMatches(result.Issues, title), nil
		}
		lastErr = err

		delay, retryable := searchRetryDelay(err, attempt)
		if !retryable || attempt == maxSearchRetries {
			break
		}
		fmt.Printf("search rate-limited, retrying in %s (attempt %d/%d): %v\n", delay, attempt+1, maxSearchRetries, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

// searchRetryDelay reports how long to wait before retrying a failed search
// and whether the error is worth retrying at all. Only rate-limit errors are
// retryable; anything else (bad credentials, a malformed query) fails fast.
func searchRetryDelay(err error, attempt int) (time.Duration, bool) {
	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		if abuseErr.RetryAfter != nil {
			return minDuration(*abuseErr.RetryAfter, searchRetryMaxDelay), true
		}
		return backoffDelay(attempt), true
	}

	var rateErr *github.RateLimitError
	if errors.As(err, &rateErr) {
		if wait := time.Until(rateErr.Rate.Reset.Time); wait > 0 {
			return minDuration(wait, searchRetryMaxDelay), true
		}
		return backoffDelay(attempt), true
	}

	return 0, false
}

func backoffDelay(attempt int) time.Duration {
	delay := searchRetryBaseDelay * time.Duration(1<<attempt)
	return minDuration(delay, searchRetryMaxDelay)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
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

// highestNumberedIssue returns the issue with the largest number, i.e. the
// most recently created one.
func highestNumberedIssue(issues []*github.Issue) *github.Issue {
	latest := issues[0]
	for _, issue := range issues[1:] {
		if issue.GetNumber() > latest.GetNumber() {
			latest = issue
		}
	}
	return latest
}

func formatCustomIssueBody(body string) string {
	if body == "" {
		return ""
	}
	return fmt.Sprintf("\n%s\n", body)
}

func formatSupersedeNote(supersededIssueNumber int) string {
	if supersededIssueNumber == 0 {
		return ""
	}
	return fmt.Sprintf("> Supersedes #%d, which was abandoned (closed with no approval or denial) before this run started.\n", supersededIssueNumber)
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
%s%s
> [!IMPORTANT]
> Required approvers:
%s

> [!TIP]
> Respond %s to continue workflow or %s to cancel.%s`,
		a.runURL(),
		formatSupersedeNote(a.supersededIssueNumber),
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
