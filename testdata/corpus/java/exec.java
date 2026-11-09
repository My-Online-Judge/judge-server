// Only judged with the real run command of the java row (security manager + /opt/oj/java.policy):
// the policy grants no permission to start a process, so the call throws SecurityException and the
// program exits 42. Without the policy the process would start.
public class Main {
    public static void main(String[] args) throws Exception {
        try {
            Runtime.getRuntime().exec("/bin/true");
        } catch (SecurityException e) {
            System.exit(42);
        }
    }
}
