package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type QueryResult struct {
	SQL      string
	Columns  []string
	Rows     [][]interface{}
	RowClass []string // CSS class per row (for error rate coloring)
	Error    error
}

// toFloat64 attempts to convert an interface{} value to float64.
func toFloat64(v interface{}) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int64:
		return float64(val), true
	case int:
		return float64(val), true
	case []byte:
		f, err := strconv.ParseFloat(string(val), 64)
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func Report(dbConnStr, replayOut, Port string) {
	if dbConnStr == "" || replayOut == "" {
		fmt.Println("Usage: ./sql-replay -mode report -db <mysql_connection_string> -replay-name <replay name> -port ':8081'")
		return
	}
	db, err := sql.Open("mysql", dbConnStr)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	queries := map[string]string{
		"Replay Summary": `select min(SUBSTRING_INDEX(file_name,'.',1)) replay_name,count(*) sql_cnts,
		            sum(case when query_time>execution_time and error_info='' then 1 else 0 end) faster_cnts,
		            sum(case when query_time<execution_time and error_info ='' then 1 else 0 end) slower_cnts,
		            sum(case when error_info<>'' then 1 else 0 end) err_cnts,
		            round(sum(case when error_info='' then ri.query_time else 0 end)/1000000/60,2) "before_sql_time(min)",
		            round(sum(case when error_info='' then ri.execution_time else 0 end)/1000000/60,2) "now_sql_time(min)"
		            from replay_info ri where ri.file_name like concat(?,'%')`,
		"Success Rate Overview": `
			SELECT
				count(*) AS total_cnt,
				sum(CASE WHEN error_info = '' THEN 1 ELSE 0 END) AS success_cnt,
				sum(CASE WHEN error_info <> '' THEN 1 ELSE 0 END) AS err_cnt,
				round(sum(CASE WHEN error_info = '' THEN 1 ELSE 0 END) * 100.0 / count(*), 2) AS success_rate_pct
			FROM replay_info
			WHERE file_name LIKE concat(?, '%')`,
		"Error by DB": `
			SELECT
				CASE WHEN db_name IS NULL OR db_name = '' THEN '(unknown)' ELSE db_name END AS db_name,
				count(*) AS total_cnt,
				sum(CASE WHEN error_info <> '' THEN 1 ELSE 0 END) AS err_cnt,
				round(sum(CASE WHEN error_info <> '' THEN 1 ELSE 0 END) * 100.0 / count(*), 2) AS err_rate_pct,
				substr(max(CONCAT(error_category, ': ', error_info)), 1, 120) AS main_error
			FROM replay_info
			WHERE file_name LIKE concat(?, '%')
			GROUP BY db_name
			ORDER BY err_cnt DESC`,
		"Sample1: <500us": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) <= 500
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample2: 500us~1ms": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 500 AND AVG(query_time) <= 1000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample3: 1ms~10ms": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 1000 AND AVG(query_time) <= 10000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample4: 10ms~100ms": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 10000 AND AVG(query_time) <= 100000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample5: 100ms~1s": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 100000 AND AVG(query_time) <= 1000000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample6: 1s~10s": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		            replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 1000000 AND AVG(query_time) <= 10000000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sample7: >10s": `SELECT
		            sql_digest,max(concat(sql_type,':',ifnull(db_name,''))) sql_type,
		            COUNT(*) AS exec_cnts,
		            round(AVG(execution_time / 1000),2) AS current_ms,
		            round(AVG(query_time / 1000),2) AS before_ms,
		            concat(ROUND((AVG(execution_time / 1000) - AVG(query_time / 1000)) / AVG(query_time / 1000) ,2)*100,'%%') AS reduce_pct,
		            MIN(sql_text) AS sample_sql_text
		        FROM
		           replay_info
		        WHERE
		            file_name like concat(?,'%%') and error_info=''
		        GROUP BY
		            sql_digest
		        HAVING
		            AVG(query_time) > 10000000
		        ORDER BY
		            avg(execution_time)/avg(query_time) desc`,
		"Sql Error Info": `select sql_digest,count(*) exec_cnts,concat(ifnull(max(db_name),''),':',substr(min(error_info),1,256)) as error_info,min(sql_text) as sample_sql_text from replay_info where error_info <>'' and file_name like concat(?,'%%') group by sql_digest,substr(error_info,1,10) order by count(*) desc`,
	}

	ts_begin_query := time.Now()
	fmt.Printf("[%s] Begin execute query\n", ts_begin_query.Format("2006-01-02 15:04:05.000"))

	results := make(map[string]QueryResult)
	for name, query := range queries {
		var rows *sql.Rows
		var err error
		rows, err = db.Query(query, replayOut)
		if err != nil {
			results[name] = QueryResult{SQL: name, Error: err}
			continue
		}

		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			results[name] = QueryResult{SQL: name, Error: err}
			continue
		}

		var rowsData [][]interface{}
		for rows.Next() {
			values := make([]interface{}, len(columns))
			valuePtrs := make([]interface{}, len(columns))
			for i := range values {
				valuePtrs[i] = &values[i]
			}
			if err := rows.Scan(valuePtrs...); err != nil {
				results[name] = QueryResult{SQL: name, Error: err}
				continue
			}
			rowData := make([]interface{}, len(columns))
			for i, v := range values {
				b, ok := v.([]byte)
				if ok {
					rowData[i] = string(b)
				} else {
					rowData[i] = v
				}
			}
			rowsData = append(rowsData, rowData)
		}

		if err := rows.Err(); err != nil {
			rows.Close()
			results[name] = QueryResult{SQL: name, Error: err}
		} else {
			var rowClass []string
			if name == "Error by DB" {
				rateIdx := -1
				for i, col := range columns {
					if col == "err_rate_pct" {
						rateIdx = i
						break
					}
				}
				for _, row := range rowsData {
					cls := "err-rate-low"
					if rateIdx >= 0 && rateIdx < len(row) {
						if v, ok := toFloat64(row[rateIdx]); ok {
							if v > 10.0 {
								cls = "err-rate-high"
							} else if v >= 1.0 {
								cls = "err-rate-mid"
							}
						}
					}
					rowClass = append(rowClass, cls)
				}
			}
			results[name] = QueryResult{SQL: name, Columns: columns, Rows: rowsData, RowClass: rowClass}
		}
		rows.Close()
	}

	tmpl := `
<!DOCTYPE html>
<html>
<head>
    <title>replay report</title>
    <style>
        body {
            margin: 0;
            padding: 0;
            display: flex;
        }
        nav {
            position: fixed;
            left: 0;
            top: 0;
            height: 100%;
            width: 240px;
            background-color: #F5F5F5;
            padding: 20px;
            padding-top: 36px;
            box-sizing: border-box;
            overflow-y: auto;
        }
        nav a {
            text-decoration: none;
            color: navy;
        }
        nav ul {
            list-style: none;
            padding: 0;
            margin: 0;
        }
        nav ul li {
            margin-bottom: 10px;
        }
        main {
            flex: 1;
            padding: 20px;
            margin-left: 240px;
        }
        .blue-bar {
            background-color: white;
            color: navy;
            font-size: 20px;
            font-weight: bold;
            padding: 10px;
            margin-top: 10px;
            margin-bottom: 10px;
            width: 100%;
            box-sizing: border-box;
            text-align: left;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .nav-heading {
            font-size: 20px;
            font-weight: bold;
            color: navy;
            margin-bottom: 20px;
        }
        table {
            border-collapse: collapse;
            width: 100%;
            table-layout: fixed;
            margin-bottom: 30px;
        }
        th, td {
            border: 1px solid #ddd;
            padding: 8px;
            text-align: left;
            overflow: hidden;
            white-space: nowrap;
            text-overflow: ellipsis;
        }
        th {
            background-color: #f2f2f2;
        }
        .err-rate-high { background-color: #ffe0e0; }
        .err-rate-mid  { background-color: #fff8e0; }
        .err-rate-low  { background-color: #e0ffe0; }
        #preview {
            position: fixed;
            background-color: white;
            border: 1px solid #ccc;
            padding: 10px;
            display: none;
            z-index: 9999;
            max-width: 1000px;
            width: 500px;
            min-width: 500px;
            font-size: 15px;
        }
    </style>
</head>
<body>
    <nav>
        <ul>
            <li class="nav-heading">SQL Replay Report</li>
            {{range $key, $query := .}}
            <li><a href="#{{ $key }}">{{ $key }}</a></li>
            {{end}}
        </ul>
    </nav>
    <main>
        {{range $key, $query := .}}
        {{ if eq $key "Replay Summary" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Success Rate Overview" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Error by DB" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample1: <500us" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample2: 500us~1ms" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample3: 1ms~10ms" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample4: 10ms~100ms" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample5: 100ms~1s" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample6: 1s~10s" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sample7: >10s" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else if eq $key "Sql Error Info" }}
        <div class="blue-bar" id="{{ $key }}">{{ $key }}</div>
        {{ else }}
        <h1 id="{{ $key }}">{{ $key }}</h1>
        {{ end }}
        {{with $query.Error}}
        <p>Error: {{ . }}</p>
        {{else}}
        <table>
            <tr>
                {{range $query.Columns}}
                <th>{{.}}</th>
                {{end}}
            </tr>
                {{range $rowIdx, $rowData := $query.Rows}}
                {{ if eq $key "Error by DB" }}
                    {{ $cls := "" }}
                    {{ if lt $rowIdx (len $query.RowClass) }}
                        {{ $cls = index $query.RowClass $rowIdx }}
                    {{ end }}
                    <tr class="{{ $cls }}">
                    {{range $colIdx, $value := $rowData}}
                        {{if eq (index $query.Columns $colIdx) "sample_sql_text"}}
                            <td class="previewable">{{$value}}</td>
                        {{else if eq (index $query.Columns $colIdx) "err_rate_pct"}}
                            <td>{{$value}}%</td>
                        {{else}}
                            <td>{{$value}}</td>
                        {{end}}
                    {{end}}
                    </tr>
                {{ else }}
                <tr>
                    {{range $colIdx, $value := $rowData}}
                        {{if eq (index $query.Columns $colIdx) "sample_sql_text"}}
                            <td class="previewable">{{$value}}</td>
                        {{else}}
                            <td>{{$value}}</td>
                        {{end}}
                    {{end}}
                </tr>
                {{ end }}
                {{end}}
        </table>
        {{end}}
        {{end}}
    </main>

    <div id="preview"></div>

    <script>
        var previewableCells = document.querySelectorAll('.previewable');

        document.addEventListener('mousemove', function(event) {
            previewableCells.forEach(function(cell) {
                if (isMouseOverCell(cell, event)) {
                    showPreview(cell.textContent, event.clientX, event.clientY);
                }
            });
        });

        previewableCells.forEach(function(cell) {
            cell.addEventListener('mouseleave', function(event) {
                var preview = document.getElementById('preview');
                preview.style.display = 'none';
            });
        });

        function isMouseOverCell(cell, event) {
            var rect = cell.getBoundingClientRect();
            return event.clientX >= rect.left && event.clientX <= rect.right &&
                event.clientY >= rect.top && event.clientY <= rect.bottom;
        }

        function showPreview(content, x, y) {
            var preview = document.getElementById('preview');
            preview.innerHTML = content;
            preview.style.display = 'block';
            var rightEdge = document.body.clientWidth - x;
            if (rightEdge > 500) {
                preview.style.left = (x + 10) + 'px';
            } else {
                preview.style.left = (x - 510) + 'px';
            }
            preview.style.top = (y + 10) + 'px';
        }
    </script>

</body>
</html>
`

	t, err := template.New("webpage").Parse(tmpl)
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if err := t.Execute(w, results); err != nil {
			log.Fatal(err)
		}
	})

	ts_finsh_query := time.Now()

	fmt.Printf("[%s] Server is running on port %s\n", ts_finsh_query.Format("2006-01-02 15:04:05.000"), Port)
	if err := http.ListenAndServe(Port, nil); err != nil {
		log.Fatal(err)
	}
}
