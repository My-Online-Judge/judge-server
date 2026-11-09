<?php
// Leaves a process behind: `sleep 1000` in a session of its own (setsid), outside the process group
// the sandbox kills when a run ends. The test checks that no process of the job's uid is left once the
// job is over (the uid sweep), and that one is left when the sweep is switched off.
//
// The input "hang" makes the program wait for the job to be cancelled after it has started the
// sleeper; any other input is the sum problem, answered normally.
$f = preg_split('/\s+/', trim(stream_get_contents(STDIN)));
exec("setsid sleep 1000 >/dev/null 2>&1 &");
if ($f[0] === "hang") {
    sleep(3600);
}
echo (int)$f[0] + (int)$f[1], "\n";
