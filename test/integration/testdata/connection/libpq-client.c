/* Suite-only libpq consumer. No credential or URI is ever printed. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>

/* Public libpq ABI, declared here so the tiny fixture needs no development
 * headers. The loaded library version is checked before any URI parsing. */
typedef struct {
  char *keyword, *envvar, *compiled, *val, *label, *dispchar;
  int dispsize;
} PQconninfoOption;
typedef struct pg_conn PGconn;
typedef struct pg_result PGresult;
extern int PQlibVersion(void);
extern PQconninfoOption *PQconninfoParse(const char *, char **);
extern void PQconninfoFree(PQconninfoOption *);
extern void PQfreemem(void *);
extern PGconn *PQconnectdb(const char *);
extern int PQstatus(const PGconn *);
extern PGresult *PQexec(PGconn *, const char *);
extern int PQresultStatus(const PGresult *);
extern int PQntuples(const PGresult *);
extern char *PQgetvalue(const PGresult *, int, int);
extern void PQclear(PGresult *);
extern void PQfinish(PGconn *);

static char *line(void) {
  char *value = NULL;
  size_t capacity = 0;
  ssize_t size = getline(&value, &capacity, stdin);
  if (size < 1 || value[size - 1] != '\n') {
    free(value);
    return NULL;
  }
  value[size - 1] = '\0';
  return value;
}

int main(int argc, char **argv) {
  if (argc != 2 || PQlibVersion() != 180006) return 2;
  char *values[9] = {0};
  const char *keys[] = {"host", "port", "dbname", "user", "password", "sslmode", "sslrootcert"};
  int valid = 1;
  for (int i = 0; i < 8; i++) {
    values[i] = line();
    if (values[i] == NULL) valid = 0;
  }
  if (valid && strcmp(argv[1], "parse") == 0) {
    char *error = NULL;
    PQconninfoOption *options = PQconninfoParse(values[0], &error);
    valid = options != NULL;
    for (int i = 0; valid && i < 7; i++) {
      const char *actual = "";
      for (PQconninfoOption *p = options; p->keyword != NULL; p++) {
        if (strcmp(p->keyword, keys[i]) == 0 && p->val != NULL) actual = p->val;
      }
      valid = strcmp(actual, values[i + 1]) == 0;
    }
    if (options != NULL) PQconninfoFree(options);
    if (error != NULL) PQfreemem(error);
  } else if (valid) {
    PGconn *connection = PQconnectdb(values[0]);
    valid = connection != NULL && PQstatus(connection) == 0;
    if (strcmp(argv[1], "deny") == 0) {
      valid = !valid;
    } else if (valid) {
      PGresult *result = PQexec(connection, "SELECT current_user,current_database()");
      valid = result != NULL && PQresultStatus(result) == 2 && PQntuples(result) == 1
        && strcmp(PQgetvalue(result, 0, 0), values[4]) == 0
        && strcmp(PQgetvalue(result, 0, 1), values[3]) == 0;
      if (result != NULL) PQclear(result);
    }
    if (connection != NULL) PQfinish(connection);
  }
  for (int i = 0; i < 8; i++) free(values[i]);
  return valid ? 0 : 1;
}
