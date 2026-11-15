# Bash output and process lifetime

On Linux and macOS, the SDK observes shell exit without reaping the shell. It
keeps reading merged stdout/stderr until EOF or one second after exit is
observed, whichever comes first. At the deadline it makes at most one final
32 KiB read. Output arriving later can be lost; continuous descendant output
cannot extend the window. A slow or blocked synchronous output callback can
extend it, and may cause more buffered output to be discarded.

The unreaped shell reserves its PID while the drain runs. Cancellation and
output-read failures can therefore signal its process group without targeting a
reused group; cancellation is disabled before reaping. Cancellation after shell
exit remains effective while draining. Normal completion does not kill
background descendants; closing the pipe may cause writers to receive SIGPIPE.
Processes that escape the group are not covered by group cancellation.

Linux uses waitid with WNOWAIT; macOS uses kqueue NOTE_EXIT. Other platforms
retain their existing fallback drain behavior. This deliberately differs from
upstream's rearming idle grace: a fixed window trades potentially truncated
late output and up to one second of latency for a bounded active drain.
