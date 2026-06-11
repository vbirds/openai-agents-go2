package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MitulShah1/openai-agents-go/review"
)

// renderMarkdown formats a built-in-schema review result as a markdown
// report suitable for posting as a PR comment.
func renderMarkdown(resp *review.Response) string {
	r := resp.Review
	var b strings.Builder

	fmt.Fprintf(&b, "# Code Review: %s\n\n", verdictLabel(r.Verdict))
	fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(r.Summary))
	fmt.Fprintf(&b, "**Confidence:** %.0f%% &nbsp;|&nbsp; **Findings:** %d &nbsp;|&nbsp; **Model:** %s\n\n",
		r.Confidence*100, len(r.Findings), resp.Model)

	if len(r.Findings) == 0 {
		b.WriteString("No findings. :white_check_mark:\n")
		return b.String()
	}

	findings := make([]review.Finding, len(r.Findings))
	copy(findings, r.Findings)
	sort.SliceStable(findings, func(i, j int) bool {
		return findings[i].Severity.Rank() < findings[j].Severity.Rank()
	})

	b.WriteString("## Findings\n\n")
	for i, f := range findings {
		fmt.Fprintf(&b, "### %d. %s %s — %s\n\n", i+1, severityBadge(f.Severity), strings.ToUpper(string(f.Severity)), f.Title)
		fmt.Fprintf(&b, "`%s`%s &nbsp;·&nbsp; %s &nbsp;·&nbsp; confidence %.0f%%\n\n",
			f.File, lineRef(f), f.Category, f.Confidence*100)
		fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(f.Body))
		if strings.TrimSpace(f.Suggestion) != "" {
			fmt.Fprintf(&b, "**Suggestion:**\n\n%s\n\n", formatSuggestion(f.Suggestion))
		}
	}
	return b.String()
}

func verdictLabel(v review.Verdict) string {
	switch v {
	case review.VerdictApprove:
		return "Approve :white_check_mark:"
	case review.VerdictRequestChanges:
		return "Request Changes :x:"
	case review.VerdictComment:
		return "Comments :speech_balloon:"
	default:
		return string(v)
	}
}

func severityBadge(s review.Severity) string {
	switch s {
	case review.SeverityCritical:
		return ":rotating_light:"
	case review.SeverityHigh:
		return ":red_circle:"
	case review.SeverityMedium:
		return ":orange_circle:"
	case review.SeverityLow:
		return ":yellow_circle:"
	default:
		return ":information_source:"
	}
}

func lineRef(f review.Finding) string {
	switch {
	case f.LineStart <= 0:
		return ""
	case f.LineEnd > f.LineStart:
		return fmt.Sprintf(":%d-%d", f.LineStart, f.LineEnd)
	default:
		return fmt.Sprintf(":%d", f.LineStart)
	}
}

// formatSuggestion wraps multi-line suggestions in a code fence; short prose
// stays inline.
func formatSuggestion(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "```") {
		return s
	}
	if strings.Contains(s, "\n") {
		return "```suggestion\n" + s + "\n```"
	}
	return s
}
