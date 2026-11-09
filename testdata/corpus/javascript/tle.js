// Case 1 never ends (CPU time limit); the others answer.
const [a, b] = require('fs').readFileSync(0, 'utf8').trim().split(/\s+/).map(Number);
if (a === 1 && b === 2) {
    for (;;) {}
}
console.log(a + b);
