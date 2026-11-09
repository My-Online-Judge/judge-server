// Leaves a process behind: `sleep 1000` in a session of its own (detached: setsid), outside the process
// group the sandbox kills when a run ends. The test checks that no process of the job's uid is left once
// the job is over (the uid sweep), and that one is left when the sweep is switched off.
//
// The input "hang" makes the program wait for the job to be cancelled after it has started the
// sleeper; any other input is the sum problem, answered normally.
const { spawn } = require('child_process');
const f = require('fs').readFileSync(0, 'utf8').trim().split(/\s+/);
spawn('/bin/sleep', ['1000'], { detached: true, stdio: 'ignore' }).unref();
if (f[0] === 'hang') {
  setTimeout(() => {}, 3600 * 1000);
} else {
  console.log(Number(f[0]) + Number(f[1]));
}
