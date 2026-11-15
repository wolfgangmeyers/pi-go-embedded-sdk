# Bash output and process lifetime

On Linux and macOS, the SDK observes shell exit without reaping. It reads merged
stdout/stderr until EOF or the hard post-exit limit. Quiet periods do not end the drain.
At two seconds after
exit observation the drain closes the pipe and returns `ErrBashOutputIncomplete`
if EOF has not been observed. This reports possible truncation even when no bytes
are immediately available. A final read preserves some buffered output; a slow
or blocked synchronous callback can delay completion and cannot be interrupted.

The unreaped shell reserves its PID while draining. Cancellation and read
failures can signal its process group without targeting a reused group. The hard
limit itself does not kill descendants; closing the pipe can cause SIGPIPE.
Cancellation remains effective during drain, and abort or timeout classification
wins over the incomplete-output error. Cancellation is disabled before reaping.
Processes that escape the group are not covered by group cancellation.

Linux uses waitid with WNOWAIT; macOS uses kqueue NOTE_EXIT. Other platforms
retain the original 100 ms rearming idle fallback without the hard limit.
