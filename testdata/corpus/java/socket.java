// Only judged with the real run command of the java row (security manager + /opt/oj/java.policy):
// the policy grants no SocketPermission, so the connect throws SecurityException and the program
// exits 42. (Any other failure, such as a refused connection, exits 1.)
public class Main {
    public static void main(String[] args) {
        try {
            new java.net.Socket("127.0.0.1", 9).close();
        } catch (SecurityException e) {
            System.exit(42);
        } catch (java.io.IOException e) {
            System.exit(1);
        }
    }
}
