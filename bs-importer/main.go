package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	plugin "github.com/Entropy-Increase-Team/skinloom/sdk/go"
)

//go:embed migrations
var migrationsFS embed.FS

type jobStream struct {
	mu      sync.Mutex
	history []string
	subs    map[chan string]struct{}
	closed  bool
}

func newJobStream() *jobStream {
	return &jobStream{subs: make(map[chan string]struct{})}
}

func (s *jobStream) publish(event, msg string) {
	payload, _ := json.Marshal(map[string]any{
		"event": event,
		"time":  time.Now().UTC().Format(time.RFC3339),
		"msg":   msg,
	})
	line := fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(payload))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	const maxStreamHistory = 500
	if len(s.history) >= maxStreamHistory {
		copy(s.history, s.history[1:])
		s.history[len(s.history)-1] = line
	} else {
		s.history = append(s.history, line)
	}
	for ch := range s.subs {
		select {
		case ch <- line:
		default:
		}
	}
}

func (s *jobStream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for ch := range s.subs {
		close(ch)
	}
	s.subs = nil
}

func (s *jobStream) subscribe() ([]string, chan string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hist := append([]string(nil), s.history...)
	if s.closed {
		return hist, nil, true
	}
	ch := make(chan string, 64)
	s.subs[ch] = struct{}{}
	return hist, ch, false
}

func (s *jobStream) unsubscribe(ch chan string) {
	if ch == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.subs != nil {
		delete(s.subs, ch)
	}
}

// JobSummary is returned by GET /admin/jobs and GET /admin/jobs/:id.
type JobSummary struct {
	ID            int64         `json:"id"`
	Mode          string        `json:"mode"`
	DryRun        bool          `json:"dryRun"`
	Tables        []string      `json:"tables"`
	Status        string        `json:"status"`
	ImportedCount int           `json:"importedCount"`
	SkippedCount  int           `json:"skippedCount"`
	ErrorCount    int           `json:"errorCount"`
	Report        *ImportReport `json:"report,omitempty"`
	CreatedBy     int64         `json:"createdBy"`
	CreatedAt     string        `json:"createdAt"`
	FinishedAt    string        `json:"finishedAt,omitempty"`
}

// BSImporterPlugin is the Tier 2 Native Go plugin for importing Blessing Skin third-party extension tables.
type BSImporterPlugin struct {
	ctx       *plugin.Context
	streamsMu sync.Mutex
	streams   map[int64]*jobStream
}

func (p *BSImporterPlugin) streamFor(jobID int64, create bool) *jobStream {
	p.streamsMu.Lock()
	defer p.streamsMu.Unlock()
	if p.streams == nil {
		p.streams = make(map[int64]*jobStream)
	}
	st, ok := p.streams[jobID]
	if !ok && create {
		// Evict only the oldest closed stream once map grows beyond 20 entries so
		// recently completed jobs stay replayable for connecting SSE clients.
		for len(p.streams) >= 20 {
			var oldestID int64 = -1
			for id, old := range p.streams {
				old.mu.Lock()
				isClosed := old.closed
				old.mu.Unlock()
				if isClosed && (oldestID == -1 || id < oldestID) {
					oldestID = id
				}
			}
			if oldestID == -1 {
				break
			}
			delete(p.streams, oldestID)
		}
		st = newJobStream()
		p.streams[jobID] = st
	}
	return st
}

func isZhLocale(locale string) bool {
	l := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(locale, "_", "-")))
	return l == "" || l == "zh" || strings.HasPrefix(l, "zh-")
}

func (p *BSImporterPlugin) tr(reqCtx context.Context, locale, key string, params map[string]any, zhFallback, enFallback string) string {
	if p != nil && p.ctx != nil && p.ctx.I18n != nil {
		if out, err := p.ctx.I18n.T(reqCtx, key, params, locale); err == nil && out != "" && out != key {
			return out
		}
	}
	if isZhLocale(locale) {
		return zhFallback
	}
	return enFallback
}

func (p *BSImporterPlugin) localizeMode(reqCtx context.Context, locale, mode string) string {
	switch mode {
	case "db":
		return p.tr(reqCtx, locale, "modeDbShort", nil, "直连数据库", "Direct DB")
	case "bridge":
		return p.tr(reqCtx, locale, "modeBridgeShort", nil, "导出桥接", "Export Bridge")
	default:
		return mode
	}
}

func (p *BSImporterPlugin) localizeStatus(reqCtx context.Context, locale, status string) string {
	switch status {
	case "completed":
		return p.tr(reqCtx, locale, "statusCompleted", nil, "已完成", "Completed")
	case "completed_with_errors":
		return p.tr(reqCtx, locale, "statusCompletedWithErrors", nil, "部分失败", "Completed with errors")
	case "running":
		return p.tr(reqCtx, locale, "statusRunning", nil, "执行中", "Running")
	case "failed":
		return p.tr(reqCtx, locale, "statusFailed", nil, "失败", "Failed")
	default:
		return status
	}
}

func (p *BSImporterPlugin) localizeBool(reqCtx context.Context, locale string, val bool) string {
	if val {
		return p.tr(reqCtx, locale, "yes", nil, "是", "Yes")
	}
	return p.tr(reqCtx, locale, "no", nil, "否", "No")
}

func (p *BSImporterPlugin) Setup(ctx *plugin.Context) error {
	p.ctx = ctx
	db := ctx.DB()
	if db == nil {
		return fmt.Errorf("bs-importer requires isolated SQL storage")
	}
	if err := db.Migrate(context.Background(), migrationsFS, "migrations"); err != nil {
		return fmt.Errorf("migrate bs-importer db: %w", err)
	}

	// 0. List all available migration providers (built-in legacy tables + dynamically discovered plugins).
	ctx.HTTP.Get("/admin/providers", func(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		discovered := discoverMigrationProviders(reqCtx, ctx)
		items := make([]ProviderSummary, 0, len(legacyBuiltInList)+len(discovered))
		for _, tbl := range legacyBuiltInList {
			items = append(items, ProviderSummary{
				ID:           tbl,
				TargetPlugin: targetPluginForLegacyTable(tbl),
				Dynamic:      false,
			})
		}
		for _, prov := range discovered {
			items = append(items, ProviderSummary{
				ID:             prov.ID,
				TargetPlugin:   prov.TargetPlugin,
				Dynamic:        true,
				Label:          prov.Label,
				Description:    prov.Description,
				OptionKeys:     prov.OptionKeys,
				OptionPrefixes: prov.OptionPrefixes,
				Tables:         prov.Tables,
			})
		}
		return plugin.JSON(http.StatusOK, map[string]any{"items": items}), nil
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	// 1. Test source connectivity and inspect available BS extension tables & plugin migration options.
	ctx.HTTP.Post("/admin/test", func(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		var cfg SourceConfig
		if err := req.DecodeJSON(&cfg); err != nil {
			msg := p.tr(reqCtx, req.Locale, "errInvalidJson", map[string]any{"error": err.Error()},
				"请求体 JSON 格式无效："+err.Error(), "Invalid JSON payload: "+err.Error())
			return nil, plugin.Reject(msg)
		}
		src, err := openSource(reqCtx, cfg)
		if err != nil {
			return nil, plugin.Reject(err.Error())
		}
		defer src.Close()
		providers := discoverMigrationProviders(reqCtx, ctx)
		res, err := src.Probe(reqCtx, providers)
		if err != nil {
			return nil, plugin.Reject(err.Error())
		}
		return plugin.JSON(http.StatusOK, res), nil
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	// 2. Run extension table/provider import (sync or dry-run) and persist job history in isolated SQL DB.
	ctx.HTTP.Post("/admin/run", func(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		var cfg SourceConfig
		if err := req.DecodeJSON(&cfg); err != nil {
			msg := p.tr(reqCtx, req.Locale, "errInvalidJson", map[string]any{"error": err.Error()},
				"请求体 JSON 格式无效："+err.Error(), "Invalid JSON payload: "+err.Error())
			return nil, plugin.Reject(msg)
		}
		providers := discoverMigrationProviders(reqCtx, ctx)
		tables, provByID, err := normalizeRequestedTargets(cfg.Tables, providers)
		if err != nil {
			return nil, plugin.Reject(err.Error())
		}
		src, err := openSource(reqCtx, cfg)
		if err != nil {
			return nil, plugin.Reject(err.Error())
		}
		defer src.Close()

		tablesJSON, _ := json.Marshal(tables)
		var actorID int64
		if req.User != nil {
			actorID = req.User.UserID
		}
		jobID, err := p.insertJob(reqCtx, cfg.Mode, cfg.DryRun, string(tablesJSON), actorID)
		if err != nil {
			return nil, err
		}

		stream := p.streamFor(jobID, true)
		defer stream.close()
		jobFinished := false
		defer func() {
			if !jobFinished {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				failRep := &ImportReport{DryRun: cfg.DryRun, TotalErrors: 1}
				_ = p.finishJob(cleanupCtx, jobID, "failed", failRep)
			}
		}()

		locale := req.Locale
		translate := func(key string, params map[string]any, zhFallback, enFallback string) string {
			return p.tr(reqCtx, locale, key, params, zhFallback, enFallback)
		}
		emit := func(msg string) {
			stream.publish("progress", msg)
		}
		modeText := p.localizeMode(reqCtx, locale, cfg.Mode)
		dryRunText := p.localizeBool(reqCtx, locale, cfg.DryRun)
		emit(translate("logJobStarted", map[string]any{
			"jobId":  jobID,
			"mode":   modeText,
			"dryRun": dryRunText,
		},
			fmt.Sprintf("已启动任务 #%d（模式：%s，模拟演练：%s）", jobID, modeText, dryRunText),
			fmt.Sprintf("Started job #%d (mode=%s, dryRun=%s)", jobID, modeText, dryRunText),
		))

		report := ImportReport{
			DryRun: cfg.DryRun,
			Tables: make([]TableImportReport, 0, len(tables)),
		}
		for _, tbl := range tables {
			var tblRep TableImportReport
			switch tbl {
			case TableInvitationCodes:
				rows, rErr := src.ReadInvitationCodes(reqCtx)
				if rErr != nil {
					tblRep = TableImportReport{
						Table:        tbl,
						TargetPlugin: "invitation-codes",
						Errors:       []string{rErr.Error()},
					}
				} else {
					tblRep = dispatchBatches(reqCtx, ctx, "invitation-codes", "importCodes", rows, cfg.DryRun, emit, translate)
					tblRep.Table = tbl
				}
			case TableMojangVerification:
				rows, rErr := src.ReadMojangVerifications(reqCtx)
				if rErr != nil {
					tblRep = TableImportReport{
						Table:        tbl,
						TargetPlugin: "mojang-verification",
						Errors:       []string{rErr.Error()},
					}
				} else {
					tblRep = dispatchBatches(reqCtx, ctx, "mojang-verification", "importVerifications", rows, cfg.DryRun, emit, translate)
					tblRep.Table = tbl
				}
			case TableTextureDescription:
				rows, rErr := src.ReadTextureDescriptions(reqCtx)
				if rErr != nil {
					tblRep = TableImportReport{
						Table:        tbl,
						TargetPlugin: "texture-description",
						Errors:       []string{rErr.Error()},
					}
				} else {
					tblRep = dispatchBatches(reqCtx, ctx, "texture-description", "importDescriptions", rows, cfg.DryRun, emit, translate)
					tblRep.Table = tbl
				}
			default:
				if prov, ok := provByID[tbl]; ok {
					tblRep = dispatchGenericProvider(reqCtx, ctx, src, prov, cfg.DryRun, emit, translate)
				}
			}
			report.Tables = append(report.Tables, tblRep)
			report.TotalRead += tblRep.Read
			report.TotalImported += tblRep.Imported
			report.TotalSkipped += tblRep.Skipped
			report.TotalErrors += len(tblRep.Errors)
		}

		status := "completed"
		if report.TotalErrors > 0 {
			status = "completed_with_errors"
		}
		if err := p.finishJob(reqCtx, jobID, status, &report); err != nil {
			return nil, err
		}
		jobFinished = true
		stream.publish("done", translate("logJobFinished", map[string]any{
			"jobId":    jobID,
			"imported": report.TotalImported,
			"skipped":  report.TotalSkipped,
			"errors":   report.TotalErrors,
		},
			fmt.Sprintf("任务 #%d 已结束：成功 %d，跳过 %d，错误 %d", jobID, report.TotalImported, report.TotalSkipped, report.TotalErrors),
			fmt.Sprintf("Finished job #%d: imported=%d skipped=%d errors=%d", jobID, report.TotalImported, report.TotalSkipped, report.TotalErrors),
		))

		_ = ctx.Audit.Record(reqCtx, "bs_importer.run", "job", strconv.FormatInt(jobID, 10), map[string]any{
			"mode":     cfg.Mode,
			"dryRun":   cfg.DryRun,
			"tables":   tables,
			"imported": report.TotalImported,
			"skipped":  report.TotalSkipped,
			"errors":   report.TotalErrors,
		})

		return plugin.JSON(http.StatusOK, map[string]any{
			"jobId":  jobID,
			"status": status,
			"report": report,
		}), nil
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	// 3. List historical migration jobs.
	ctx.HTTP.Get("/admin/jobs", func(reqCtx context.Context, _ *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		jobs, err := p.listJobs(reqCtx, 30)
		if err != nil {
			return nil, err
		}
		return plugin.JSON(http.StatusOK, map[string]any{"items": jobs}), nil
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	// 4. Get full job detail including per-table report.
	ctx.HTTP.Get("/admin/jobs/:id", func(reqCtx context.Context, req *plugin.HTTPRequest) (*plugin.HTTPResponse, error) {
		jobID, err := strconv.ParseInt(req.Params["id"], 10, 64)
		if err != nil || jobID <= 0 {
			return nil, plugin.Reject(p.tr(reqCtx, req.Locale, "errInvalidJobId", nil, "无效的任务 ID。", "Invalid job ID."))
		}
		job, err := p.getJob(reqCtx, jobID)
		if err != nil {
			return plugin.JSON(http.StatusNotFound, map[string]any{
				"detail": p.tr(reqCtx, req.Locale, "errJobNotFound", nil, "未找到对应的迁移任务。", "Migration job not found."),
			}), nil
		}
		return plugin.JSON(http.StatusOK, job), nil
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	// 5. Real-time Server-Sent Events (SSE) progress stream via Tier 2 local stream proxy.
	ctx.HTTP.StreamFunc(http.MethodGet, "/admin/jobs/:id/stream", func(w http.ResponseWriter, r *http.Request) {
		viewer := plugin.ProxyViewer(r)
		if !viewer.Authenticated || viewer.Role != "superadmin" {
			http.Error(w, "super administrators only", http.StatusForbidden)
			return
		}
		params := plugin.StreamParams(r)
		locale := plugin.ProxyLocale(r)
		jobID, err := strconv.ParseInt(params["id"], 10, 64)
		if err != nil || jobID <= 0 {
			http.Error(w, p.tr(r.Context(), locale, "errInvalidJobId", nil, "无效的任务 ID。", "Invalid job ID."), http.StatusBadRequest)
			return
		}

		st := p.streamFor(jobID, false)
		if st == nil {
			job, qErr := p.getJob(r.Context(), jobID)
			if qErr != nil {
				http.Error(w, p.tr(r.Context(), locale, "errJobNotFound", nil, "未找到对应的迁移任务。", "Migration job not found."), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)
			statusText := p.localizeStatus(r.Context(), locale, job.Status)
			msg := p.tr(r.Context(), locale, "logJobStatus", map[string]any{
				"jobId":    job.ID,
				"status":   statusText,
				"imported": job.ImportedCount,
				"skipped":  job.SkippedCount,
				"errors":   job.ErrorCount,
			},
				fmt.Sprintf("任务 #%d 状态：%s（成功 %d，跳过 %d，错误 %d）", job.ID, statusText, job.ImportedCount, job.SkippedCount, job.ErrorCount),
				fmt.Sprintf("Job #%d status: %s (imported=%d, skipped=%d, errors=%d)", job.ID, statusText, job.ImportedCount, job.SkippedCount, job.ErrorCount),
			)
			payload, _ := json.Marshal(map[string]any{
				"event":  "done",
				"status": job.Status,
				"msg":    msg,
			})
			_, _ = fmt.Fprintf(w, "event: done\ndata: %s\n\n", string(payload))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)

		hist, ch, closed := st.subscribe()
		defer st.unsubscribe(ch)
		for _, line := range hist {
			_, _ = w.Write([]byte(line))
		}
		if flusher != nil {
			flusher.Flush()
		}
		if closed || ch == nil {
			return
		}
		for {
			select {
			case <-r.Context().Done():
				return
			case line, ok := <-ch:
				if !ok {
					return
				}
				_, _ = w.Write([]byte(line))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}, plugin.WithAuth(plugin.AuthSuperadmin))

	return nil
}

func formatSQLTimestamp(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.UTC().Format(time.RFC3339)
	case []byte:
		return normalizeTimestampString(string(x))
	case string:
		return normalizeTimestampString(x)
	default:
		return normalizeTimestampString(fmt.Sprint(x))
	}
}

func (p *BSImporterPlugin) insertJob(ctx context.Context, mode string, dryRun bool, tablesJSON string, actorID int64) (int64, error) {
	db := p.ctx.DB()
	if db.Dialect() == "postgres" {
		var id int64
		err := db.QueryRow(ctx,
			`INSERT INTO {{prefix}}jobs (mode, dry_run, tables_json, status, report_json, created_by) VALUES (?, ?, ?, 'running', '{}', ?) RETURNING id`,
			mode, dryRun, tablesJSON, actorID,
		).Scan(&id)
		return id, err
	}
	res, err := db.Exec(ctx,
		`INSERT INTO {{prefix}}jobs (mode, dry_run, tables_json, status, report_json, created_by) VALUES (?, ?, ?, 'running', '{}', ?)`,
		mode, dryRun, tablesJSON, actorID,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (p *BSImporterPlugin) finishJob(ctx context.Context, jobID int64, status string, report *ImportReport) error {
	reportRaw, _ := json.Marshal(report)
	_, err := p.ctx.DB().Exec(ctx,
		`UPDATE {{prefix}}jobs SET status = ?, imported_count = ?, skipped_count = ?, error_count = ?, report_json = ?, finished_at = CURRENT_TIMESTAMP WHERE id = ?`,
		status, report.TotalImported, report.TotalSkipped, report.TotalErrors, string(reportRaw), jobID,
	)
	return err
}

func (p *BSImporterPlugin) listJobs(ctx context.Context, limit int) ([]JobSummary, error) {
	rows, err := p.ctx.DB().Query(ctx,
		`SELECT id, mode, dry_run, tables_json, status, imported_count, skipped_count, error_count, created_by, created_at, finished_at FROM {{prefix}}jobs ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []JobSummary{}
	for rows.Next() {
		var j JobSummary
		var dryRaw, createdRaw, finishedRaw any
		var tablesJSON string
		if err := rows.Scan(&j.ID, &j.Mode, &dryRaw, &tablesJSON, &j.Status, &j.ImportedCount, &j.SkippedCount, &j.ErrorCount, &j.CreatedBy, &createdRaw, &finishedRaw); err != nil {
			return nil, err
		}
		j.DryRun = parseSQLBool(dryRaw)
		j.CreatedAt = formatSQLTimestamp(createdRaw)
		j.FinishedAt = formatSQLTimestamp(finishedRaw)
		_ = json.Unmarshal([]byte(tablesJSON), &j.Tables)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (p *BSImporterPlugin) getJob(ctx context.Context, jobID int64) (*JobSummary, error) {
	var j JobSummary
	var dryRaw, createdRaw, finishedRaw any
	var tablesJSON, reportJSON string
	err := p.ctx.DB().QueryRow(ctx,
		`SELECT id, mode, dry_run, tables_json, status, imported_count, skipped_count, error_count, report_json, created_by, created_at, finished_at FROM {{prefix}}jobs WHERE id = ?`,
		jobID,
	).Scan(&j.ID, &j.Mode, &dryRaw, &tablesJSON, &j.Status, &j.ImportedCount, &j.SkippedCount, &j.ErrorCount, &reportJSON, &j.CreatedBy, &createdRaw, &finishedRaw)
	if err != nil {
		return nil, err
	}
	j.DryRun = parseSQLBool(dryRaw)
	j.CreatedAt = formatSQLTimestamp(createdRaw)
	j.FinishedAt = formatSQLTimestamp(finishedRaw)
	_ = json.Unmarshal([]byte(tablesJSON), &j.Tables)
	if reportJSON != "" {
		var rep ImportReport
		if json.Unmarshal([]byte(reportJSON), &rep) == nil {
			j.Report = &rep
		}
	}
	return &j, nil
}

func main() {
	plugin.Serve(&BSImporterPlugin{})
}
