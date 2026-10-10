// Suite-only actual pgJDBC parser/consumer. Never print a URI, properties or exceptions.
import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.sql.DriverManager;
import java.util.Properties;
import java.util.logging.LogManager;
import org.postgresql.Driver;

class JdbcClient {
  public static void main(String[] args) {
    LogManager.getLogManager().reset();
    try {
      var input = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
      var values = new String[8];
      for (int i = 0; i < values.length; i++) {
        values[i] = input.readLine();
        if (values[i] == null) System.exit(2);
      }
      Properties actual = Driver.parseURL(values[0], new Properties());
      String[] keys = {"PGHOST", "PGPORT", "PGDBNAME", "user", "password", "sslmode", "sslrootcert"};
      boolean valid = actual != null;
      for (int i = 0; valid && i < keys.length; i++) {
        String expected = values[i + 1];
        if (i == 0 && expected.contains(":")) expected = "[" + expected + "]";
        valid = expected.equals(actual.getProperty(keys[i], ""));
      }
      if (!valid) System.exit(1);
      if (!args[0].equals("parse")) {
        boolean connected = false;
        try (var connection = DriverManager.getConnection(values[0]);
             var statement = connection.createStatement();
             var result = statement.executeQuery("SELECT current_user,current_database()")) {
          connected = result.next() && result.getString(1).equals(values[4])
            && result.getString(2).equals(values[3]);
        } catch (Exception ignored) {
          connected = false;
        }
        valid = args[0].equals("deny") ? !connected : connected;
      }
      System.exit(valid ? 0 : 1);
    } catch (Exception ignored) {
      System.exit(2);
    }
  }
}
