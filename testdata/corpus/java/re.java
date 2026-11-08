public class Main {
    public static void main(String[] args) {
        throw new IllegalStateException("boom"); // uncaught: exit code 1
    }
}
