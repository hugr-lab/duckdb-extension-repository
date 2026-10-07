// runner: opens an in-memory DuckDB with startup options and runs statements for the e2e harness.
//
//   runner [name=value ...] < statements
//
// Each argument is a startup option set through duckdb_set_config, before the database opens, so
// options that DuckDB accepts only at startup (allow_extension_repositories, extension_directory)
// work. Statements on stdin are separated by NUL bytes. For each one a JSON line is written:
//
//   {"stmt":"...","ok":true,"columns":["a"],"rows":[["1"]]}
//   {"stmt":"...","ok":false,"error":"..."}
//
// Values are rendered as strings (null as JSON null). The C API is used so that the runner has no
// C++ ABI coupling to the library it links.
#include "duckdb.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void json_string(FILE *out, const char *s) {
	fputc('"', out);
	for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
		switch (*p) {
		case '"':
			fputs("\\\"", out);
			break;
		case '\\':
			fputs("\\\\", out);
			break;
		case '\n':
			fputs("\\n", out);
			break;
		case '\r':
			fputs("\\r", out);
			break;
		case '\t':
			fputs("\\t", out);
			break;
		default:
			if (*p < 0x20) {
				fprintf(out, "\\u%04x", *p);
			} else {
				fputc(*p, out);
			}
		}
	}
	fputc('"', out);
}

// read_statement reads up to the next NUL byte or EOF. It returns NULL at EOF with nothing read.
static char *read_statement(FILE *in) {
	size_t cap = 4096, len = 0;
	char *buf = malloc(cap);
	int c;
	while ((c = fgetc(in)) != EOF && c != '\0') {
		if (len + 1 >= cap) {
			cap *= 2;
			buf = realloc(buf, cap);
		}
		buf[len++] = (char)c;
	}
	if (c == EOF && len == 0) {
		free(buf);
		return NULL;
	}
	buf[len] = '\0';
	return buf;
}

static void run(duckdb_connection con, const char *stmt) {
	duckdb_result res;
	FILE *out = stdout;
	fputs("{\"stmt\":", out);
	json_string(out, stmt);
	if (duckdb_query(con, stmt, &res) == DuckDBError) {
		const char *err = duckdb_result_error(&res);
		fputs(",\"ok\":false,\"error\":", out);
		json_string(out, err ? err : "unknown error");
		fputs("}\n", out);
		duckdb_destroy_result(&res);
		fflush(out);
		return;
	}
	idx_t cols = duckdb_column_count(&res);
	idx_t rows = duckdb_row_count(&res);
	fputs(",\"ok\":true,\"columns\":[", out);
	for (idx_t c = 0; c < cols; c++) {
		if (c) {
			fputc(',', out);
		}
		json_string(out, duckdb_column_name(&res, c));
	}
	fputs("],\"rows\":[", out);
	for (idx_t r = 0; r < rows; r++) {
		fputs(r ? ",[" : "[", out);
		for (idx_t c = 0; c < cols; c++) {
			if (c) {
				fputc(',', out);
			}
			if (duckdb_value_is_null(&res, c, r)) {
				fputs("null", out);
			} else {
				char *v = duckdb_value_varchar(&res, c, r);
				json_string(out, v ? v : "");
				duckdb_free(v);
			}
		}
		fputc(']', out);
	}
	fputs("]}\n", out);
	duckdb_destroy_result(&res);
	fflush(out);
}

int main(int argc, char **argv) {
	duckdb_config config;
	if (duckdb_create_config(&config) == DuckDBError) {
		fprintf(stderr, "runner: cannot create config\n");
		return 2;
	}
	for (int i = 1; i < argc; i++) {
		char *eq = strchr(argv[i], '=');
		if (!eq) {
			fprintf(stderr, "runner: option %s is not name=value\n", argv[i]);
			return 2;
		}
		*eq = '\0';
		if (duckdb_set_config(config, argv[i], eq + 1) == DuckDBError) {
			fprintf(stderr, "runner: cannot set %s\n", argv[i]);
			return 2;
		}
	}
	duckdb_database db;
	char *open_err = NULL;
	if (duckdb_open_ext(NULL, &db, config, &open_err) == DuckDBError) {
		fprintf(stderr, "runner: open failed: %s\n", open_err ? open_err : "unknown");
		duckdb_free(open_err);
		return 2;
	}
	duckdb_destroy_config(&config);
	duckdb_connection con;
	if (duckdb_connect(db, &con) == DuckDBError) {
		fprintf(stderr, "runner: connect failed\n");
		return 2;
	}
	char *stmt;
	while ((stmt = read_statement(stdin)) != NULL) {
		if (strspn(stmt, " \t\r\n") != strlen(stmt)) {
			run(con, stmt);
		}
		free(stmt);
	}
	duckdb_disconnect(&con);
	duckdb_close(&db);
	return 0;
}
