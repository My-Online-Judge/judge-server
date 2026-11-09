import java.io.BufferedReader;
import java.io.IOException;
import java.io.InputStreamReader;
import java.util.StringTokenizer;

// Case 1 never ends (CPU time limit); the others answer.
public class Main {
    static volatile long spin;

    public static void main(String[] args) throws IOException {
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in));
        StringTokenizer st = new StringTokenizer(in.readLine());
        long a = Long.parseLong(st.nextToken());
        long b = Long.parseLong(st.nextToken());
        if (a == 1 && b == 2) {
            for (;;) {
                spin++;
            }
        }
        System.out.println(a + b);
    }
}
