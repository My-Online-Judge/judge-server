<?php
// Case 1 never ends (CPU time limit); the others answer.
fscanf(STDIN, "%d %d", $a, $b);
if ($a == 1 && $b == 2) {
    while (true) {
    }
}
echo $a + $b, "\n";
