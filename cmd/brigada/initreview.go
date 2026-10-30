package main

import "fmt"

// initReviewIntro is the introduction's sentence on the effective
// review.family policy.
func initReviewIntro(requireFamily bool) string {
	if requireFamily {
		return "(review.family=require: with no other family, a review waits)."
	}
	return "when one is available (review.family=prefer; the same family or agent\notherwise)."
}

// initReviewWorkers is the workers step's sentence on the effective
// review.family policy.
func initReviewWorkers(requireFamily bool) string {
	if requireFamily {
		return " Reviews always go\n  to a different model family than the author's (review.family=require),\n  so you need at least two families (e.g. claude + codex).\n"
	}
	return " Reviews prefer\n  a different model family than the author's (review.family=prefer, the\n  default); with one family they fall back to the same family or the same\n  agent. Two families (e.g. claude + codex) give cross-family reviews.\n"
}

// initReviewsLine is the Plan's Reviews summary under the effective
// review.family policy.
func initReviewsLine(families int, requireFamily bool) string {
	switch {
	case families >= 2:
		return fmt.Sprintf("%d model families, so every task can get a reviewer of another one.", families)
	case requireFamily:
		return fmt.Sprintf("%d model family; review.family=require, so reviews wait for another family.", families)
	}
	return fmt.Sprintf("%d model family; review.family=prefer, so reviews prefer another family and fall back to the same family or the same agent.", families)
}
