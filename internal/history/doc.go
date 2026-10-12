// Package history keeps everything that happened and makes it readable: one
// timeline of conversation items per project (user and foreman turns, tool
// calls and their results, task specs and results, questions and answers,
// reviews, notifications), full session transcripts uploaded as artifacts
// after every job, and the append-only event log that messages and transcripts
// reference. Retention is configurable; the default keeps everything.
package history
