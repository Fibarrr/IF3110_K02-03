(function () {
    "use strict";

    let csrfToken = "";
    let currentUser = null;

    function readCookie(name) {
        const prefix = name + "=";
        const parts = document.cookie ? document.cookie.split("; ") : [];
        for (let i = 0; i < parts.length; i++) {
            if (parts[i].indexOf(prefix) === 0) {
                return decodeURIComponent(parts[i].substring(prefix.length));
            }
        }
        return "";
    }

    function getCSRFToken() {
        return csrfToken || readCookie("if3110_csrf");
    }

    function showAlert(message, type) {
        const el = document.getElementById("global-alert");
        if (!el) return;
        if (!message) {
            el.hidden = true;
            el.textContent = "";
            return;
        }
        el.className = "alert " + (type === "success" ? "alert-success" : type === "info" ? "alert-info" : "alert-error");
        el.textContent = message;
        el.hidden = false;
    }

    async function apiFetch(path, options) {
        const opts = Object.assign({ credentials: "same-origin" }, options || {});
        const headers = Object.assign({}, opts.headers || {});
        const method = (opts.method || "GET").toUpperCase();
        if (method !== "GET" && method !== "HEAD") {
            const token = getCSRFToken();
            if (token) {
                headers["X-CSRF-Token"] = token;
            }
            if (opts.body && typeof opts.body === "object" && !(opts.body instanceof FormData)) {
                headers["Content-Type"] = "application/json";
                opts.body = JSON.stringify(opts.body);
            }
        }
        opts.headers = headers;
        const response = await fetch(path, opts);
        let payload = null;
        const contentType = response.headers.get("Content-Type") || "";
        if (contentType.indexOf("application/json") !== -1) {
            try {
                payload = await response.json();
            } catch (_) {
                payload = null;
            }
        }
        if (!response.ok) {
            const msg = (payload && payload.error) ? payload.error : ("Request gagal (" + response.status + ")");
            const err = new Error(msg);
            err.status = response.status;
            err.payload = payload;
            throw err;
        }
        return payload;
    }

    function escapeHTML(value) {
        if (value === null || value === undefined) return "";
        return String(value)
            .replace(/&/g, "&amp;")
            .replace(/</g, "&lt;")
            .replace(/>/g, "&gt;")
            .replace(/"/g, "&quot;")
            .replace(/'/g, "&#39;");
    }

    function formatAccessLabel(access) {
        if (access === "view_edit") return "View + Edit";
        if (access === "view_only") return "View Only";
        return "Tidak Ada Akses";
    }

    async function loadSession(redirectIfUnauthenticated) {
        try {
            const data = await apiFetch("/api/auth/me");
            currentUser = data.user;
            csrfToken = data.csrfToken || readCookie("if3110_csrf");

            const userEl = document.getElementById("current-username");
            if (userEl && currentUser) {
                userEl.textContent = currentUser.username;
            }
            const roleEl = document.getElementById("current-role-badge");
            if (roleEl && currentUser) {
                roleEl.hidden = false;
                roleEl.textContent = currentUser.platformAdmin ? "Platform Admin" : "Pengguna";
                roleEl.className = "badge " + (currentUser.platformAdmin ? "badge-success" : "badge-neutral");
            }
            const adminNav = document.getElementById("nav-admin-link");
            if (adminNav) {
                adminNav.hidden = !(currentUser && currentUser.platformAdmin);
            }
            const adminCta = document.getElementById("workspace-admin-cta");
            if (adminCta) {
                adminCta.hidden = !(currentUser && currentUser.platformAdmin);
            }
            return currentUser;
        } catch (err) {
            if (redirectIfUnauthenticated && err.status === 401) {
                window.location.href = "/login";
            }
            return null;
        }
    }

    function bindLogout() {
        const btn = document.getElementById("logout-btn");
        if (!btn) return;
        btn.addEventListener("click", async function () {
            try {
                await apiFetch("/api/auth/logout", { method: "POST" });
            } catch (_) {
                // ignore
            }
            window.location.href = "/login";
        });
    }

    // ---------------- Page: Login ----------------
    async function initLoginPage() {
        const user = await loadSession(false);
        if (user) {
            window.location.href = "/";
            return;
        }
        const form = document.getElementById("login-form");
        if (!form) return;
        form.addEventListener("submit", async function (e) {
            e.preventDefault();
            showAlert("");
            const username = document.getElementById("login-username").value.trim();
            const password = document.getElementById("login-password").value;
            try {
                const res = await apiFetch("/api/auth/login", {
                    method: "POST",
                    body: { username: username, password: password }
                });
                csrfToken = res.csrfToken || "";
                window.location.href = "/";
            } catch (err) {
                showAlert(err.message, "error");
            }
        });
    }

    // ---------------- Page: Workspace (Select Database) ----------------
    async function initWorkspacePage() {
        const user = await loadSession(true);
        if (!user) return;

        const grid = document.getElementById("accessible-databases-grid");
        const refreshBtn = document.getElementById("refresh-databases-btn");

        async function loadDatabases() {
            if (!grid) return;
            grid.innerHTML = '<div class="card muted">Memuat daftar database...</div>';
            try {
                const data = await apiFetch("/api/databases");
                const list = (data && data.databases) ? data.databases : [];
                if (list.length === 0) {
                    const adminHint = user.platformAdmin
                        ? ' Anda adalah <strong>Platform Admin</strong> — buka <a href="/admin">Panel Admin</a> untuk mendaftarkan koneksi database PostgreSQL, membuat grup, memasukkan akun Anda ke grup, dan memberikan akses <code>View Only</code> atau <code>View + Edit</code>.'
                        : ' Hubungi Platform Admin untuk menambahkan akun Anda ke grup yang memiliki akses database.';
                    grid.innerHTML = '<div class="card"><p>Belum ada database yang dapat diakses oleh akun Anda.</p><p class="muted">' + adminHint + '</p></div>';
                    return;
                }
                grid.innerHTML = list.map(function (db) {
                    const badgeClass = db.access === "view_edit" ? "badge-success" : "badge-warning";
                    return (
                        '<article class="card db-card">' +
                        '  <div>' +
                        '    <div class="db-card-title">' +
                        '      <h3>' + escapeHTML(db.name) + '</h3>' +
                        '      <span class="badge ' + badgeClass + '">' + escapeHTML(formatAccessLabel(db.access)) + '</span>' +
                        '    </div>' +
                        '    <p class="muted">' + escapeHTML(db.description || "Tanpa deskripsi") + '</p>' +
                        '  </div>' +
                        '  <div class="db-meta-list">' +
                        '    <div><strong>ID:</strong> <code>' + escapeHTML(db.id) + '</code></div>' +
                        '    <div><strong>Target:</strong> <code>' + escapeHTML(db.host + ":" + db.port + "/" + db.databaseName) + '</code></div>' +
                        '    <div><strong>User / SSL:</strong> ' + escapeHTML(db.username) + ' (' + escapeHTML(db.sslMode) + ')</div>' +
                        '  </div>' +
                        '  <div>' +
                        '    <a class="btn btn-primary btn-block" href="/databases/' + encodeURIComponent(db.id) + '">Buka Database Explorer</a>' +
                        '  </div>' +
                        '</article>'
                    );
                }).join("");
            } catch (err) {
                showAlert(err.message, "error");
                grid.innerHTML = '<div class="card muted">Gagal memuat daftar database.</div>';
            }
        }

        if (refreshBtn) {
            refreshBtn.addEventListener("click", loadDatabases);
        }
        await loadDatabases();
    }

    // ---------------- Page: Database Explorer ----------------
    async function initDatabasePage(databaseID) {
        const user = await loadSession(true);
        if (!user) return;

        let cachedSchemas = [];
        let graphDetails = [];
        let graphLoadPromise = null;
        let selectedSchema = "";
        let selectedTable = "";
        let selectedTableDetails = null;
        let currentOffset = 0;
        let tableLoadSeq = 0;
        let rowsLoadSeq = 0;
        let migrationCanEdit = false;
        let migrationList = [];
        let selectedMigrationId = null;

        const titleEl = document.getElementById("db-header-title");
        const metaEl = document.getElementById("db-header-meta");
        const accessBadge = document.getElementById("db-access-badge");
        const connBadge = document.getElementById("db-conn-badge");
        const schemaTree = document.getElementById("schema-tree");
        const filterInput = document.getElementById("schema-filter-input");

        const workspaceTabButtons = document.querySelectorAll(".workspace-tab-btn");
        workspaceTabButtons.forEach(function (btn) {
            btn.addEventListener("click", function () {
                const target = btn.getAttribute("data-workspace-tab");
                workspaceTabButtons.forEach(function (item) { item.classList.toggle("active", item === btn); });
                document.querySelectorAll(".workspace-panel").forEach(function (panel) {
                    panel.hidden = panel.id !== "workspace-" + target;
                });
                if (target === "migrations") loadMigrationHistory();
                if (target === "visualizer") loadSchemaGraph();
                if (target === "table-editor") {
                    if (selectedSchema && selectedTable) loadTableRows(selectedSchema, selectedTable);
                    updateTableEditorControls();
                }
            });
        });

        const graphFilter = document.getElementById("visualizer-filter-input");
        const graphReload = document.getElementById("visualizer-reload-btn");
        const graphNodes = document.getElementById("schema-graph-nodes");
        const graphViewport = document.getElementById("schema-graph-viewport");
        const graphStatus = document.getElementById("schema-graph-status");

        function drawSchemaGraphEdges() {
            const svg = document.getElementById("schema-graph-edges");
            const canvas = document.getElementById("schema-graph-canvas");
            if (!svg || !canvas || !graphNodes) return;
            const width = canvas.scrollWidth;
            const height = canvas.scrollHeight;
            svg.setAttribute("width", width);
            svg.setAttribute("height", height);
            svg.setAttribute("viewBox", "0 0 " + width + " " + height);
            const positions = new Map();
            graphNodes.querySelectorAll(".schema-graph-node").forEach(function (node) {
                const key = node.getAttribute("data-key");
                const box = node.getBoundingClientRect();
                const root = canvas.getBoundingClientRect();
                positions.set(key, {
                    left: box.left - root.left,
                    right: box.right - root.left,
                    top: box.top - root.top,
                    bottom: box.bottom - root.top,
                    centerX: box.left - root.left + box.width / 2,
                    centerY: box.top - root.top + box.height / 2
                });
            });
            const paths = [];
            graphDetails.forEach(function (table) {
                const fromKey = table.schema + "." + table.name;
                const from = positions.get(fromKey);
                if (!from) return;
                (table.constraints || []).filter(function (con) {
                    return con.type === "FOREIGN KEY" && con.referencedTable;
                }).forEach(function (con) {
                    const to = positions.get((con.referencedSchema || table.schema) + "." + con.referencedTable);
                    if (!to) return;
                    const dx = to.centerX - from.centerX;
                    const dy = to.centerY - from.centerY;
                    if (Math.abs(dy) > Math.abs(dx)) {
                        const down = dy > 0;
                        const y1 = down ? from.bottom : from.top;
                        const y2 = down ? to.top : to.bottom;
                        const bend = Math.max(35, Math.abs(y2 - y1) * 0.45);
                        const c1 = y1 + (down ? bend : -bend);
                        const c2 = y2 - (down ? bend : -bend);
                        paths.push('<path class="schema-graph-edge" d="M ' + from.centerX + ' ' + y1 + ' C ' + from.centerX + ' ' + c1 + ', ' + to.centerX + ' ' + c2 + ', ' + to.centerX + ' ' + y2 + '" marker-end="url(#schema-graph-arrow)"/>');
                    } else {
                        const forward = dx > 0;
                        const x1 = forward ? from.right : from.left;
                        const x2 = forward ? to.left : to.right;
                        const bend = Math.max(35, Math.abs(x2 - x1) * 0.45);
                        const c1 = x1 + (forward ? bend : -bend);
                        const c2 = x2 - (forward ? bend : -bend);
                        paths.push('<path class="schema-graph-edge" d="M ' + x1 + ' ' + from.centerY + ' C ' + c1 + ' ' + from.centerY + ', ' + c2 + ' ' + to.centerY + ', ' + x2 + ' ' + to.centerY + '" marker-end="url(#schema-graph-arrow)"/>');
                    }
                });
            });
            svg.innerHTML = '<defs><marker id="schema-graph-arrow" markerWidth="8" markerHeight="8" refX="7" refY="4" orient="auto"><path d="M0,0 L8,4 L0,8 z" fill="#64748b"/></marker></defs>' + paths.join("");
        }

        function renderSchemaGraph() {
            if (!graphNodes) return;
            const query = graphFilter ? graphFilter.value.trim().toLowerCase() : "";
            const visible = graphDetails.filter(function (table) {
                return !query || (table.schema + "." + table.name).toLowerCase().includes(query);
            });
            const visibleKeys = new Set(visible.map(function (table) { return table.schema + "." + table.name; }));
            const gridRows = Math.max(1, Math.ceil(Math.sqrt(visible.length)));
            graphNodes.style.gridTemplateRows = "repeat(" + gridRows + ", auto)";
            graphNodes.style.gridTemplateColumns = "repeat(" + Math.ceil(visible.length / gridRows) + ", 290px)";
            graphNodes.style.gridAutoFlow = "column";
            if (visible.length === 0) {
                graphNodes.innerHTML = '<div class="muted schema-graph-empty">Tidak ada tabel yang cocok untuk ditampilkan.</div>';
                drawSchemaGraphEdges();
                return;
            }
            graphNodes.innerHTML = visible.map(function (table) {
                const key = table.schema + "." + table.name;
                const columns = table.columns || [];
                const foreignColumns = new Set();
                (table.constraints || []).forEach(function (constraint) {
                    if (constraint.type === "FOREIGN KEY") (constraint.columns || []).forEach(function (column) { foreignColumns.add(column); });
                });
                const columnHTML = columns.length ? columns.map(function (column) {
                    const keyClass = column.isPrimaryKey ? " graph-primary-key" : (foreignColumns.has(column.name) ? " graph-foreign-key" : "");
                    const keyMark = foreignColumns.has(column.name) && !column.isPrimaryKey ? "↗ " : "";
                    return '<li class="' + keyClass + '"><span>' + keyMark + escapeHTML(column.name) + '</span><span>' + escapeHTML(column.dataType) + '</span></li>';
                }).join("") : '<li class="muted">Tidak ada kolom</li>';
                const references = (table.constraints || []).filter(function (con) {
                    return con.type === "FOREIGN KEY" && con.referencedTable && visibleKeys.has((con.referencedSchema || table.schema) + "." + con.referencedTable);
                }).length;
                return '<article class="schema-graph-node" tabindex="0" role="button" data-key="' + escapeHTML(key) + '" data-schema="' + escapeHTML(table.schema) + '" data-table="' + escapeHTML(table.name) + '">' +
                    '<header class="schema-graph-node-header"><strong>' + escapeHTML(table.name) + '</strong><span>' + escapeHTML(table.schema) + '</span></header>' +
                    '<ul class="schema-graph-columns">' + columnHTML + '</ul>' +
                    (references ? '<div class="schema-graph-node-header"><span>' + references + ' relasi FK</span></div>' : '') + '</article>';
            }).join("");
            requestAnimationFrame(drawSchemaGraphEdges);
            graphNodes.querySelectorAll(".schema-graph-node").forEach(function (node) {
                function openTable() {
                    selectedSchema = node.getAttribute("data-schema");
                    selectedTable = node.getAttribute("data-table");
                    currentOffset = 0;
                    document.querySelectorAll(".workspace-tab-btn").forEach(function (tab) {
                        tab.classList.toggle("active", tab.getAttribute("data-workspace-tab") === "explorer");
                    });
                    document.querySelectorAll(".workspace-panel").forEach(function (panel) {
                        panel.hidden = panel.id !== "workspace-explorer";
                    });
                    renderSchemaTree();
                    loadSelectedTable();
                }
                node.addEventListener("click", openTable);
                node.addEventListener("keydown", function (event) {
                    if (event.key === "Enter" || event.key === " ") { event.preventDefault(); openTable(); }
                });
            });
        }

        async function loadSchemaGraph(forceReload) {
            if (!forceReload && graphDetails.length > 0) {
                if (graphViewport) graphViewport.hidden = false;
                renderSchemaGraph();
                return;
            }
            if (graphLoadPromise) return graphLoadPromise;
            if (graphStatus) graphStatus.textContent = "Memuat metadata tabel dan relasi Foreign Key...";
            graphLoadPromise = (async function () {
                try {
                    if (cachedSchemas.length === 0) {
                        const data = await apiFetch("/api/databases/" + encodeURIComponent(databaseID) + "/schemas");
                        cachedSchemas = data.schemas || [];
                    }
                    const tables = [];
                    cachedSchemas.forEach(function (schema) {
                        (schema.tables || []).forEach(function (table) { tables.push({ schema: schema.name, name: table.name }); });
                    });
                    const details = new Array(tables.length);
                    let nextTable = 0;
                    let loadedTables = 0;
                    const workers = Array.from({ length: Math.min(4, tables.length) }, async function () {
                        while (nextTable < tables.length) {
                            const index = nextTable++;
                            const table = tables[index];
                            const path = "/api/databases/" + encodeURIComponent(databaseID) + "/schemas/" + encodeURIComponent(table.schema) + "/tables/" + encodeURIComponent(table.name);
                            details[index] = await apiFetch(path);
                            loadedTables++;
                            if (graphStatus) graphStatus.textContent = "Memuat metadata tabel " + loadedTables + "/" + tables.length + "...";
                        }
                    });
                    await Promise.all(workers);
                    graphDetails = details;
                    if (graphStatus) graphStatus.textContent = details.length + " tabel ditampilkan; garis menunjukkan relasi Foreign Key.";
                    if (graphViewport) graphViewport.hidden = false;
                    renderSchemaGraph();
                } catch (err) {
                    if (graphStatus) graphStatus.textContent = "Gagal memuat visualizer: " + err.message;
                    if (graphViewport) graphViewport.hidden = true;
                } finally {
                    graphLoadPromise = null;
                }
            })();
            return graphLoadPromise;
        }

        if (graphFilter) graphFilter.addEventListener("input", renderSchemaGraph);
        if (graphReload) graphReload.addEventListener("click", function () { graphDetails = []; loadSchemaGraph(true); });
        window.addEventListener("resize", function () { if (graphViewport && !graphViewport.hidden) drawSchemaGraphEdges(); });

        function updateTableEditorControls() {
            const title = document.getElementById("table-editor-title");
            const addButton = document.getElementById("table-row-add-btn");
            if (title) title.textContent = selectedSchema && selectedTable ? selectedSchema + "." + selectedTable : "Pilih tabel di Schema & Table Viewer";
            if (addButton) addButton.hidden = !(selectedSchema && selectedTable && migrationCanEdit);
            renderStructureEditor();
        }

        function quoteSQLIdentifier(value) { return '"' + String(value).replace(/"/g, '""') + '"'; }

        function renderStructureEditor() {
            const op = document.getElementById("structure-operation");
            const current = document.getElementById("structure-current-column");
            const newField = document.getElementById("structure-new-column-field");
            const typeField = document.getElementById("structure-data-type-field");
            const nullableField = document.getElementById("structure-nullable-field");
            const defaultField = document.getElementById("structure-default-field");
            const currentField = document.getElementById("structure-current-column-field");
            const preview = document.getElementById("structure-sql-preview");
            const form = document.getElementById("structure-editor-form");
            if (!op || !preview || !form) return;
            const operation = op.value;
            const previousColumn = current ? current.value : "";
            if (current) current.innerHTML = (selectedTableDetails && selectedTableDetails.columns || []).map(function (col) {
                return '<option value="' + escapeHTML(col.name) + '">' + escapeHTML(col.name) + ' (' + escapeHTML(col.dataType) + ')</option>';
            }).join("");
            if (current && previousColumn) current.value = previousColumn;
            if (currentField) currentField.hidden = operation === "add-column";
            if (newField) newField.hidden = operation === "drop-column";
            if (typeField) typeField.hidden = operation !== "add-column";
            if (nullableField) nullableField.hidden = operation !== "add-column";
            if (defaultField) defaultField.hidden = operation !== "add-column";
            form.querySelectorAll("input,select,button").forEach(function (el) { el.disabled = !migrationCanEdit || !selectedTable; });
            if (!selectedTable) { preview.textContent = "Pilih tabel di Schema & Table Viewer."; return; }
            const table = quoteSQLIdentifier(selectedSchema) + "." + quoteSQLIdentifier(selectedTable);
            let sql = "";
            if (operation === "add-column") {
                const name = document.getElementById("structure-new-column").value.trim();
                const type = document.getElementById("structure-data-type").value.trim();
                if (!name || !type) { preview.textContent = "Isi nama kolom dan tipe datanya untuk melihat SQL."; return; }
                if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || !/^[A-Za-z][A-Za-z0-9_ ]*(\(\s*\d+(\s*,\s*\d+)?\s*\))?(\[\])?$/.test(type)) {
                    preview.textContent = "Nama kolom atau tipe data belum valid."; return;
                }
                sql = "ALTER TABLE " + table + " ADD COLUMN " + quoteSQLIdentifier(name) + " " + type;
                const defaultValue = document.getElementById("structure-default").value.trim();
                if (defaultValue) sql += " DEFAULT " + defaultValue;
                if (!document.getElementById("structure-nullable").checked) sql += " NOT NULL";
                sql += ";";
            } else if (operation === "rename-column") {
                const oldName = current ? current.value : "";
                const newName = document.getElementById("structure-new-column").value.trim();
                if (!newName || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(newName)) { preview.textContent = "Isi nama baru yang valid."; return; }
                sql = "ALTER TABLE " + table + " RENAME COLUMN " + quoteSQLIdentifier(oldName) + " TO " + quoteSQLIdentifier(newName) + ";";
            } else {
                sql = "ALTER TABLE " + table + " DROP COLUMN " + quoteSQLIdentifier(current ? current.value : "") + ";";
            }
            preview.textContent = sql;
        }

        ["structure-operation", "structure-current-column", "structure-new-column", "structure-data-type", "structure-nullable", "structure-default"].forEach(function (id) {
            const element = document.getElementById(id);
            if (element) element.addEventListener("input", renderStructureEditor);
            if (element && element.tagName === "SELECT") element.addEventListener("change", renderStructureEditor);
        });

        const rowForm = document.getElementById("table-row-form");
        const rowFields = document.getElementById("table-row-fields");
        const rowAddButton = document.getElementById("table-row-add-btn");
        const rowCancelButton = document.getElementById("table-row-cancel-btn");
        let rowFormMode = "create";
        let rowFormKey = {};

        function openTableRowForm(mode, row) {
            if (!rowForm || !rowFields || !selectedTableDetails) return;
            rowFormMode = mode;
            rowFormKey = {};
            const primaryKeys = (selectedTableDetails.columns || []).filter(function (column) { return column.isPrimaryKey; });
            if (mode === "edit" && primaryKeys.length === 0) { showAlert("Baris hanya bisa diedit jika tabel memiliki primary key.", "error"); return; }
            primaryKeys.forEach(function (column) { rowFormKey[column.name] = row[column.name]; });
            rowFields.innerHTML = (selectedTableDetails.columns || []).map(function (column) {
                const isKey = column.isPrimaryKey;
                const value = row && row[column.name] !== null && row[column.name] !== undefined ? String(row[column.name]) : "";
                const isNull = !!row && (row[column.name] === null || row[column.name] === undefined);
                return '<div class="field table-row-field"><label class="' + (isKey ? 'primary-key-label' : '') + '" for="row-value-' + escapeHTML(column.name) + '">' +
                    escapeHTML(column.name) + ' <span class="muted">(' + escapeHTML(column.dataType) + ')</span></label>' +
                    '<input id="row-value-' + escapeHTML(column.name) + '" data-row-column="' + escapeHTML(column.name) + '" type="text" value="' + escapeHTML(value) + '" placeholder="kosong = DEFAULT" ' + (isKey && mode === "edit" ? 'disabled' : '') + '>' +
                    '<label class="checkbox-label"><input type="checkbox" data-row-null="' + escapeHTML(column.name) + '" ' + (isNull ? 'checked' : '') + '> NULL</label></div>';
            }).join("");
            rowForm.hidden = false;
            rowForm.scrollIntoView({ behavior: "smooth", block: "nearest" });
        }

        if (rowAddButton) rowAddButton.addEventListener("click", function () { openTableRowForm("create", null); });
        if (rowCancelButton) rowCancelButton.addEventListener("click", function () { if (rowForm) rowForm.hidden = true; });

        if (rowForm) rowForm.addEventListener("submit", async function (event) {
            event.preventDefault();
            if (!migrationCanEdit || !selectedSchema || !selectedTable) return;
            const values = {};
            rowFields.querySelectorAll("[data-row-column]").forEach(function (input) {
                const name = input.getAttribute("data-row-column");
                if (input.disabled) return;
                const nullInput = rowFields.querySelector('[data-row-null="' + CSS.escape(name) + '"]');
                if (nullInput && nullInput.checked) values[name] = null;
                else if (input.value !== "") values[name] = input.value;
            });
            const suffix = "/schemas/" + encodeURIComponent(selectedSchema) + "/tables/" + encodeURIComponent(selectedTable) + "/rows";
            const editing = rowFormMode === "edit";
            try {
                await apiFetch("/api/databases/" + encodeURIComponent(databaseID) + suffix, {
                    method: editing ? "PUT" : "POST",
                    body: editing ? { key: rowFormKey, values: values } : { values: values }
                });
                rowForm.hidden = true;
                showAlert("Baris berhasil disimpan.", "success");
                await loadTableRows(selectedSchema, selectedTable);
            } catch (err) { showAlert(err.message, "error"); }
        });

        const structureForm = document.getElementById("structure-editor-form");
        if (structureForm) structureForm.addEventListener("submit", async function (event) {
            event.preventDefault();
            if (!migrationCanEdit || !selectedTable) return;
            const sql = (document.getElementById("structure-sql-preview").textContent || "").trim();
            const name = document.getElementById("structure-migration-name").value.trim();
            if (!sql || sql.indexOf(";") === -1 || sql.indexOf("valid") !== -1 || sql.indexOf("Isi ") === 0) { showAlert("Lengkapi perubahan struktur terlebih dahulu.", "error"); return; }
            try {
                const result = await apiFetch(migrationPath(""), { method: "POST", body: { name: name, sql: sql } });
                showAlert("Draft migration dibuat. Buka Migration Editor untuk meninjau dan menjalankannya.", "success");
                if (result && result.migration) migrationList.unshift(result.migration);
                document.querySelectorAll(".workspace-tab-btn").forEach(function (tab) { tab.classList.toggle("active", tab.getAttribute("data-workspace-tab") === "migrations"); });
                document.querySelectorAll(".workspace-panel").forEach(function (panel) { panel.hidden = panel.id !== "workspace-migrations"; });
                loadMigrationHistory();
            } catch (err) { showAlert(err.message, "error"); }
        });

        const tabButtons = document.querySelectorAll(".tab-btn");
        tabButtons.forEach(function (btn) {
            btn.addEventListener("click", function () {
                const target = btn.getAttribute("data-tab");
                tabButtons.forEach(function (b) { b.classList.toggle("active", b === btn); });
                ["columns", "constraints", "indexes", "rows"].forEach(function (name) {
                    const panel = document.getElementById("tab-" + name);
                    if (panel) panel.hidden = (name !== target);
                });
            });
        });

        function renderSchemaTree() {
            if (!schemaTree) return;
            const query = (filterInput ? filterInput.value : "").trim().toLowerCase();
            if (!cachedSchemas || cachedSchemas.length === 0) {
                schemaTree.innerHTML = '<p class="muted">Tidak ada schema pengguna yang ditemukan.</p>';
                return;
            }
            const html = cachedSchemas.map(function (schema) {
                const tables = (schema.tables || []).filter(function (tbl) {
                    if (!query) return true;
                    return schema.name.toLowerCase().indexOf(query) !== -1 || tbl.name.toLowerCase().indexOf(query) !== -1;
                });
                if (query && tables.length === 0 && schema.name.toLowerCase().indexOf(query) === -1) {
                    return "";
                }
                const itemsHTML = tables.length === 0
                    ? '<li class="muted" style="padding:0.25rem 0.5rem;">(Kosong)</li>'
                    : tables.map(function (tbl) {
                        const isActive = (schema.name === selectedSchema && tbl.name === selectedTable);
                        return (
                            '<li>' +
                            '  <button type="button" class="table-item-btn ' + (isActive ? 'active' : '') + '" data-schema="' + escapeHTML(schema.name) + '" data-table="' + escapeHTML(tbl.name) + '">' +
                            '    <span>' + escapeHTML(tbl.name) + '</span>' +
                            '    <span class="badge badge-neutral">' + escapeHTML(tbl.type === "VIEW" ? "VIEW" : tbl.estimatedRows) + '</span>' +
                            '  </button>' +
                            '</li>'
                        );
                    }).join("");
                return (
                    '<div class="schema-group">' +
                    '  <div class="schema-group-title">' + escapeHTML(schema.name) + '</div>' +
                    '  <ul class="table-list">' + itemsHTML + '</ul>' +
                    '</div>'
                );
            }).join("");
            schemaTree.innerHTML = html || '<p class="muted">Tidak ada tabel yang cocok dengan pencarian.</p>';

            schemaTree.querySelectorAll(".table-item-btn").forEach(function (btn) {
                btn.addEventListener("click", function () {
                    selectedSchema = btn.getAttribute("data-schema");
                    selectedTable = btn.getAttribute("data-table");
                    currentOffset = 0;
                    renderSchemaTree();
                    loadSelectedTable();
                });
            });
        }

        if (filterInput) {
            filterInput.addEventListener("input", renderSchemaTree);
        }

        async function loadSelectedTable() {
            const targetSchema = selectedSchema;
            const targetTable = selectedTable;
            if (!targetSchema || !targetTable) return;
            const reqSeq = ++tableLoadSeq;
            selectedTableDetails = null;
            updateTableEditorControls();

            const tableTitle = document.getElementById("selected-table-title");
            const tableSubtitle = document.getElementById("selected-table-subtitle");
            if (tableTitle) tableTitle.textContent = targetSchema + "." + targetTable;
            if (tableSubtitle) tableSubtitle.textContent = "Memuat struktur dan data tabel...";

            try {
                const detailPath = "/api/databases/" + encodeURIComponent(databaseID) +
                    "/schemas/" + encodeURIComponent(targetSchema) +
                    "/tables/" + encodeURIComponent(targetTable);
                const details = await apiFetch(detailPath);
                if (reqSeq !== tableLoadSeq || selectedSchema !== targetSchema || selectedTable !== targetTable) {
                    return;
                }
                if (tableSubtitle) {
                    tableSubtitle.textContent = details.type + " — " +
                        (details.columns ? details.columns.length : 0) + " kolom, " +
                        (details.constraints ? details.constraints.length : 0) + " constraint, " +
                        (details.indexes ? details.indexes.length : 0) + " index";
                }
                selectedTableDetails = details;
                updateTableEditorControls();

                const colBody = document.getElementById("columns-tbody");
                if (colBody) {
                    const cols = details.columns || [];
                    colBody.innerHTML = cols.length === 0
                        ? '<tr><td colspan="6" class="muted">Tidak ada kolom.</td></tr>'
                        : cols.map(function (c) {
                            return (
                                '<tr>' +
                                '  <td>' + escapeHTML(c.ordinalPosition) + '</td>' +
                                '  <td><strong>' + escapeHTML(c.name) + '</strong></td>' +
                                '  <td><code>' + escapeHTML(c.dataType) + '</code></td>' +
                                '  <td>' + (c.isNullable ? "YES" : "NO") + '</td>' +
                                '  <td><code>' + escapeHTML(c.defaultValue || "-") + '</code></td>' +
                                '  <td>' + (c.isPrimaryKey ? '<span class="badge badge-success">PK</span>' : '-') + '</td>' +
                                '</tr>'
                            );
                        }).join("");
                }

                const conBody = document.getElementById("constraints-tbody");
                if (conBody) {
                    const cons = details.constraints || [];
                    conBody.innerHTML = cons.length === 0
                        ? '<tr><td colspan="4" class="muted">Tidak ada constraint pada tabel ini.</td></tr>'
                        : cons.map(function (c) {
                            return (
                                '<tr>' +
                                '  <td><strong>' + escapeHTML(c.name) + '</strong></td>' +
                                '  <td><span class="badge badge-neutral">' + escapeHTML(c.type) + '</span></td>' +
                                '  <td><code>' + escapeHTML((c.columns || []).join(", ") || "-") + '</code></td>' +
                                '  <td><code>' + escapeHTML(c.definition) + '</code></td>' +
                                '</tr>'
                            );
                        }).join("");
                }

                const idxBody = document.getElementById("indexes-tbody");
                if (idxBody) {
                    const idxs = details.indexes || [];
                    idxBody.innerHTML = idxs.length === 0
                        ? '<tr><td colspan="5" class="muted">Tidak ada index pada tabel ini.</td></tr>'
                        : idxs.map(function (idx) {
                            return (
                                '<tr>' +
                                '  <td><strong>' + escapeHTML(idx.name) + '</strong></td>' +
                                '  <td>' + (idx.isPrimary ? '<span class="badge badge-success">Ya</span>' : 'Tidak') + '</td>' +
                                '  <td>' + (idx.isUnique ? '<span class="badge badge-warning">Unik</span>' : 'Tidak') + '</td>' +
                                '  <td><code>' + escapeHTML((idx.columns || []).join(", ")) + '</code></td>' +
                                '  <td><code>' + escapeHTML(idx.definition) + '</code></td>' +
                                '</tr>'
                            );
                        }).join("");
                }

                await loadTableRows(targetSchema, targetTable);
            } catch (err) {
                if (reqSeq === tableLoadSeq) {
                    showAlert(err.message, "error");
                }
            }
        }

        async function loadTableRows(expectedSchema, expectedTable) {
            const targetSchema = expectedSchema || selectedSchema;
            const targetTable = expectedTable || selectedTable;
            if (!targetSchema || !targetTable) return;
            const rowSeq = ++rowsLoadSeq;

            const limitSelect = document.getElementById("rows-limit-select");
            const limit = limitSelect ? parseInt(limitSelect.value, 10) || 50 : 50;
            const offsetSnapshot = currentOffset;
            const infoEl = document.getElementById("rows-pagination-info");
            const prevBtn = document.getElementById("rows-prev-btn");
            const nextBtn = document.getElementById("rows-next-btn");
            const thead = document.getElementById("rows-thead");
            const tbody = document.getElementById("rows-tbody");

            try {
                const rowsPath = "/api/databases/" + encodeURIComponent(databaseID) +
                    "/schemas/" + encodeURIComponent(targetSchema) +
                    "/tables/" + encodeURIComponent(targetTable) +
                    "/rows?limit=" + limit + "&offset=" + offsetSnapshot;
                const page = await apiFetch(rowsPath);
                if (rowSeq !== rowsLoadSeq || selectedSchema !== targetSchema || selectedTable !== targetTable) {
                    return;
                }
                const cols = page.columns || [];
                const rows = page.rows || [];
                const editorHead = document.getElementById("editor-rows-thead");
                const editorBody = document.getElementById("editor-rows-tbody");
                const primaryKeys = (selectedTableDetails && selectedTableDetails.columns || []).filter(function (column) { return column.isPrimaryKey; });

                if (infoEl) {
                    const start = rows.length > 0 ? page.offset + 1 : 0;
                    const end = page.offset + rows.length;
                    infoEl.textContent = "Menampilkan " + start + "–" + end + " dari total " + page.totalRows + " baris";
                }
                if (prevBtn) prevBtn.disabled = (page.offset <= 0);
                if (nextBtn) nextBtn.disabled = !page.hasMore;

                if (thead) {
                    thead.innerHTML = "<tr>" + cols.map(function (c) {
                        return "<th>" + escapeHTML(c.name) + ' <span class="muted">(' + escapeHTML(c.databaseType) + ")</span></th>";
                    }).join("") + "</tr>";
                }
                if (tbody) {
                    if (rows.length === 0) {
                        tbody.innerHTML = '<tr><td colspan="' + Math.max(cols.length, 1) + '" class="muted">Tabel tidak memiliki baris data.</td></tr>';
                    } else {
                        tbody.innerHTML = rows.map(function (row) {
                            return "<tr>" + cols.map(function (c) {
                                const val = row[c.name];
                                if (val === null || val === undefined) {
                                    return '<td><span class="badge badge-neutral">NULL</span></td>';
                                }
                                if (typeof val === "object") {
                                    return "<td><code>" + escapeHTML(JSON.stringify(val)) + "</code></td>";
                                }
                                return "<td>" + escapeHTML(val) + "</td>";
                            }).join("") + "</tr>";
                        }).join("");
                    }
                }
                if (editorHead) editorHead.innerHTML = "<tr>" + cols.map(function (column) {
                    return "<th>" + escapeHTML(column.name) + ' <span class="muted">(' + escapeHTML(column.databaseType) + ")</span></th>";
                }).join("") + (migrationCanEdit ? "<th>Aksi</th>" : "") + "</tr>";
                if (editorBody) {
                    if (rows.length === 0) {
                        editorBody.innerHTML = '<tr><td colspan="' + Math.max(cols.length + (migrationCanEdit ? 1 : 0), 1) + '" class="muted">Tabel belum memiliki baris data.</td></tr>';
                    } else {
                        editorBody.innerHTML = rows.map(function (row, index) {
                            const cells = cols.map(function (column) {
                                const value = row[column.name];
                                if (value === null || value === undefined) return '<td><span class="badge badge-neutral">NULL</span></td>';
                                return '<td>' + escapeHTML(typeof value === "object" ? JSON.stringify(value) : value) + '</td>';
                            }).join("");
                            const actions = migrationCanEdit ? '<td class="row-action-cell"><button type="button" class="btn btn-outline btn-sm editor-row-edit" data-row-index="' + index + '" ' + (!primaryKeys.length ? 'disabled title="Tabel belum memiliki primary key"' : '') + '>Edit</button> <button type="button" class="btn btn-danger btn-sm editor-row-delete" data-row-index="' + index + '" ' + (!primaryKeys.length ? 'disabled title="Tabel belum memiliki primary key"' : '') + '>Hapus</button></td>' : '';
                            return '<tr>' + cells + actions + '</tr>';
                        }).join("");
                        editorBody.querySelectorAll(".editor-row-edit").forEach(function (button) {
                            button.addEventListener("click", function () { openTableRowForm("edit", rows[Number(button.getAttribute("data-row-index"))]); });
                        });
                        editorBody.querySelectorAll(".editor-row-delete").forEach(function (button) {
                            button.addEventListener("click", async function () {
                                const row = rows[Number(button.getAttribute("data-row-index"))];
                                const key = {};
                                primaryKeys.forEach(function (column) { key[column.name] = row[column.name]; });
                                if (!window.confirm("Hapus baris ini dari " + targetSchema + "." + targetTable + "?")) return;
                                try {
                                    const path = "/api/databases/" + encodeURIComponent(databaseID) + "/schemas/" + encodeURIComponent(targetSchema) + "/tables/" + encodeURIComponent(targetTable) + "/rows";
                                    await apiFetch(path, { method: "DELETE", body: { key: key } });
                                    showAlert("Baris berhasil dihapus.", "success");
                                    await loadTableRows(targetSchema, targetTable);
                                } catch (err) { showAlert(err.message, "error"); }
                            });
                        });
                    }
                }
            } catch (err) {
                if (rowSeq === rowsLoadSeq) {
                    showAlert(err.message, "error");
                }
            }
        }

        const limitSelect = document.getElementById("rows-limit-select");
        if (limitSelect) {
            limitSelect.addEventListener("change", function () {
                currentOffset = 0;
                loadTableRows();
            });
        }
        const prevBtn = document.getElementById("rows-prev-btn");
        if (prevBtn) {
            prevBtn.addEventListener("click", function () {
                const limit = limitSelect ? parseInt(limitSelect.value, 10) || 50 : 50;
                currentOffset = Math.max(0, currentOffset - limit);
                loadTableRows();
            });
        }
        const nextBtn = document.getElementById("rows-next-btn");
        if (nextBtn) {
            nextBtn.addEventListener("click", function () {
                const limit = limitSelect ? parseInt(limitSelect.value, 10) || 50 : 50;
                currentOffset += limit;
                loadTableRows();
            });
        }
        const refreshRowsBtn = document.getElementById("rows-refresh-btn");
        if (refreshRowsBtn) {
            refreshRowsBtn.addEventListener("click", function () {
                loadTableRows();
            });
        }

        async function loadSchemaOverview() {
            showAlert("");
            try {
                const summary = await apiFetch("/api/databases/" + encodeURIComponent(databaseID));
                const db = summary.database;
                const status = summary.status;
                const effectiveAccess = db.access || "view_only";
                migrationCanEdit = effectiveAccess === "view_edit";
                updateMigrationControls();
                updateTableEditorControls();

                if (titleEl) titleEl.textContent = db.name;
                if (metaEl) metaEl.textContent = db.host + ":" + db.port + "/" + db.databaseName + " — User: " + db.username + " (SSL: " + db.sslMode + ")";
                if (accessBadge) {
                    accessBadge.textContent = "Akses: " + formatAccessLabel(effectiveAccess);
                    accessBadge.className = "badge " + (effectiveAccess === "view_edit" ? "badge-success" : "badge-warning");
                }
                if (connBadge && status) {
                    connBadge.textContent = status.connected ? "Terhubung" : "Koneksi Gagal";
                    connBadge.className = "badge " + (status.connected ? "badge-success" : "badge-danger");
                }

                const schemaData = await apiFetch("/api/databases/" + encodeURIComponent(databaseID) + "/schemas");
                cachedSchemas = schemaData.schemas || [];
                if (!selectedSchema && cachedSchemas.length > 0) {
                    for (let i = 0; i < cachedSchemas.length; i++) {
                        if (cachedSchemas[i].tables && cachedSchemas[i].tables.length > 0) {
                            selectedSchema = cachedSchemas[i].name;
                            selectedTable = cachedSchemas[i].tables[0].name;
                            break;
                        }
                    }
                }
                renderSchemaTree();
                if (selectedSchema && selectedTable) {
                    await loadSelectedTable();
                }
            } catch (err) {
                showAlert(err.message, "error");
                if (connBadge) {
                    connBadge.textContent = "Error";
                    connBadge.className = "badge badge-danger";
                }
                if (schemaTree) {
                    schemaTree.innerHTML = '<p class="muted">' + escapeHTML(err.message) + '</p>';
                }
            }
        }

        const reloadBtn = document.getElementById("reload-schema-btn");
        if (reloadBtn) {
            reloadBtn.addEventListener("click", loadSchemaOverview);
        }

        function migrationPath(suffix) {
            return "/api/databases/" + encodeURIComponent(databaseID) + "/migrations" + (suffix || "");
        }

        function statusLabel(status) {
            const labels = { draft: "Draft", running: "Berjalan", applied: "Sukses", failed: "Gagal" };
            return labels[status] || status || "-";
        }

        function statusBadgeClass(status) {
            if (status === "applied") return "badge-success";
            if (status === "failed") return "badge-danger";
            if (status === "running") return "badge-warning";
            return "badge-neutral";
        }

        function formatMigrationDate(value) {
            if (!value) return "Belum dijalankan";
            const date = new Date(value);
            return Number.isNaN(date.getTime()) ? "-" : date.toLocaleString("id-ID");
        }

        function updateMigrationControls(migration) {
            const nameInput = document.getElementById("migration-name");
            const sqlInput = document.getElementById("migration-sql");
            const saveBtn = document.getElementById("save-migration-btn");
            const runBtn = document.getElementById("run-migration-btn");
            const newBtn = document.getElementById("new-migration-btn");
            const hint = document.getElementById("migration-access-hint");
            const status = migration ? migration.status : "draft";
            const editable = migrationCanEdit && (!migration || status === "draft" || status === "failed");

            if (nameInput) nameInput.readOnly = !editable;
            if (sqlInput) sqlInput.readOnly = !editable;
            if (saveBtn) saveBtn.hidden = !editable;
            if (newBtn) newBtn.hidden = !migrationCanEdit;
            if (runBtn) {
                runBtn.hidden = !migrationCanEdit;
                runBtn.disabled = !migrationCanEdit || !migration || (status !== "draft" && status !== "failed");
            }
            if (hint) {
                hint.textContent = migrationCanEdit
                    ? "Akses View + Edit aktif. Perubahan SQL hanya bisa disimpan sebelum migration sukses."
                    : "Akses View Only: SQL dan riwayat bisa dilihat, perubahan dan eksekusi hanya untuk View + Edit.";
            }
        }

        function renderMigrationHistory() {
            const tbody = document.getElementById("migration-history-tbody");
            if (!tbody) return;
            if (migrationList.length === 0) {
                tbody.innerHTML = '<tr><td colspan="4" class="muted">Belum ada migration untuk database ini.</td></tr>';
                return;
            }
            tbody.innerHTML = migrationList.map(function (migration) {
                const active = String(migration.id) === String(selectedMigrationId);
                const timestamp = migration.executedAt || migration.updatedAt || migration.createdAt;
                return (
                    '<tr class="migration-row ' + (active ? 'active' : '') + '" data-migration-id="' + escapeHTML(migration.id) + '" tabindex="0">' +
                    '  <td><code>V' + escapeHTML(migration.version) + '</code></td>' +
                    '  <td><strong>' + escapeHTML(migration.name) + '</strong></td>' +
                    '  <td><span class="badge ' + statusBadgeClass(migration.status) + '">' + escapeHTML(statusLabel(migration.status)) + '</span></td>' +
                    '  <td>' + escapeHTML(formatMigrationDate(timestamp)) + '</td>' +
                    '</tr>'
                );
            }).join("");
        }

        function showMigration(migration) {
            const form = document.getElementById("migration-form");
            const title = document.getElementById("migration-editor-title");
            const hint = document.getElementById("migration-editor-hint");
            const status = document.getElementById("migration-editor-status");
            const nameInput = document.getElementById("migration-name");
            const sqlInput = document.getElementById("migration-sql");
            if (!migration) {
                selectedMigrationId = null;
                if (form) form.reset();
                if (title) title.textContent = "Migration baru";
                if (hint) hint.textContent = "Tulis SQL, simpan sebagai draft, lalu jalankan setelah diperiksa.";
                if (status) {
                    status.textContent = "Draft baru";
                    status.className = "badge badge-neutral";
                }
                updateMigrationControls(null);
                renderMigrationHistory();
                return;
            }

            selectedMigrationId = migration.id;
            if (nameInput) nameInput.value = migration.name || "";
            if (sqlInput) sqlInput.value = migration.sql || "";
            if (title) title.textContent = "V" + migration.version + " - " + migration.name;
            if (hint) {
                if (migration.lastError) {
                    hint.textContent = migration.lastError;
                } else if (migration.executedAt) {
                    hint.textContent = "Dijalankan oleh user #" + migration.executedBy + " pada " + formatMigrationDate(migration.executedAt) + ".";
                } else {
                    hint.textContent = "Draft dibuat oleh user #" + migration.createdBy + " pada " + formatMigrationDate(migration.createdAt) + ".";
                }
            }
            if (status) {
                status.textContent = "V" + migration.version + " - " + statusLabel(migration.status);
                status.className = "badge " + statusBadgeClass(migration.status);
            }
            updateMigrationControls(migration);
            renderMigrationHistory();
        }

        async function loadMigrationHistory(selectID) {
            const tbody = document.getElementById("migration-history-tbody");
            if (tbody) tbody.innerHTML = '<tr><td colspan="4" class="muted">Memuat riwayat...</td></tr>';
            try {
                const data = await apiFetch(migrationPath());
                migrationList = data.migrations || [];
                renderMigrationHistory();
                const idToSelect = selectID || selectedMigrationId;
                const selected = migrationList.find(function (item) { return String(item.id) === String(idToSelect); });
                if (selected) {
                    showMigration(selected);
                } else if (idToSelect) {
                    showMigration(null);
                }
            } catch (err) {
                if (tbody) tbody.innerHTML = '<tr><td colspan="4" class="muted">Gagal memuat riwayat migration.</td></tr>';
                showAlert(err.message, "error");
            }
        }

        const historyBody = document.getElementById("migration-history-tbody");
        if (historyBody) {
            historyBody.addEventListener("click", function (event) {
                const row = event.target.closest("tr[data-migration-id]");
                if (!row) return;
                const selected = migrationList.find(function (item) { return String(item.id) === row.getAttribute("data-migration-id"); });
                if (selected) showMigration(selected);
            });
            historyBody.addEventListener("keydown", function (event) {
                if (event.key !== "Enter" && event.key !== " ") return;
                const row = event.target.closest("tr[data-migration-id]");
                if (!row) return;
                event.preventDefault();
                const selected = migrationList.find(function (item) { return String(item.id) === row.getAttribute("data-migration-id"); });
                if (selected) showMigration(selected);
            });
        }

        const migrationForm = document.getElementById("migration-form");
        if (migrationForm) {
            migrationForm.addEventListener("submit", async function (event) {
                event.preventDefault();
                if (!migrationCanEdit) return;
                const input = {
                    name: document.getElementById("migration-name").value.trim(),
                    sql: document.getElementById("migration-sql").value
                };
                const isUpdate = selectedMigrationId !== null;
                const path = isUpdate ? migrationPath("/" + encodeURIComponent(selectedMigrationId)) : migrationPath();
                try {
                    const saved = await apiFetch(path, { method: isUpdate ? "PUT" : "POST", body: input });
                    showAlert("Draft migration V" + saved.version + " berhasil disimpan.", "success");
                    await loadMigrationHistory(saved.id);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const newMigrationBtn = document.getElementById("new-migration-btn");
        if (newMigrationBtn) newMigrationBtn.addEventListener("click", function () { showMigration(null); });

        const runMigrationBtn = document.getElementById("run-migration-btn");
        if (runMigrationBtn) {
            runMigrationBtn.addEventListener("click", async function () {
                if (!migrationCanEdit || selectedMigrationId === null) return;
                const migration = migrationList.find(function (item) { return String(item.id) === String(selectedMigrationId); });
                if (!migration || (migration.status !== "draft" && migration.status !== "failed")) return;
                if (!window.confirm("Jalankan migration V" + migration.version + " pada database ini? SQL yang sukses akan mengubah database target.")) return;
                runMigrationBtn.disabled = true;
                try {
                    const result = await apiFetch(migrationPath("/" + encodeURIComponent(selectedMigrationId) + "/execute"), { method: "POST" });
                    showAlert("Migration V" + result.version + " berhasil dijalankan.", "success");
                    await loadMigrationHistory(result.id);
                } catch (err) {
                    const failedMigration = err.payload && err.payload.migration;
                    if (failedMigration) {
                        await loadMigrationHistory(failedMigration.id);
                    } else {
                        await loadMigrationHistory();
                    }
                    showAlert(err.message, "error");
                } finally {
                    updateMigrationControls(migrationList.find(function (item) { return String(item.id) === String(selectedMigrationId); }));
                }
            });
        }

        const refreshMigrationsBtn = document.getElementById("refresh-migrations-btn");
        if (refreshMigrationsBtn) refreshMigrationsBtn.addEventListener("click", function () { loadMigrationHistory(); });

        await loadSchemaOverview();
    }

    // ---------------- Page: Platform Admin Console ----------------
    async function initAdminPage() {
        const user = await loadSession(true);
        if (!user) return;
        if (!user.platformAdmin) {
            showAlert("Halaman ini khusus untuk Platform Admin.", "error");
            return;
        }

        let adminDatabases = [];
        let adminUsers = [];
        let adminGroups = [];
        let selectedGroupID = null;
        let selectedGroupName = "";

        const dbTbody = document.getElementById("admin-databases-tbody");
        const groupsTbody = document.getElementById("admin-groups-tbody");
        const usersTbody = document.getElementById("admin-users-tbody");
        const editingIDInput = document.getElementById("db-form-editing-id");
        const dbIDInput = document.getElementById("db-id");
        const dbSubmitBtn = document.getElementById("db-submit-btn");
        const dbPwdHint = document.getElementById("db-password-hint");

        function resetDatabaseForm() {
            const form = document.getElementById("db-connection-form");
            if (form) form.reset();
            if (editingIDInput) editingIDInput.value = "";
            if (dbIDInput) dbIDInput.disabled = false;
            if (dbSubmitBtn) dbSubmitBtn.textContent = "Simpan Koneksi Database";
            if (dbPwdHint) dbPwdHint.textContent = "";
        }

        const resetBtn = document.getElementById("reset-db-form-btn");
        if (resetBtn) {
            resetBtn.addEventListener("click", resetDatabaseForm);
        }

        async function loadAdminDatabases() {
            const data = await apiFetch("/api/admin/databases");
            adminDatabases = data.databases || [];
            if (dbTbody) {
                if (adminDatabases.length === 0) {
                    dbTbody.innerHTML = '<tr><td colspan="6" class="muted">Belum ada koneksi database terdaftar.</td></tr>';
                } else {
                    dbTbody.innerHTML = adminDatabases.map(function (db) {
                        return (
                            '<tr>' +
                            '  <td><code>' + escapeHTML(db.id) + '</code></td>' +
                            '  <td><strong>' + escapeHTML(db.name) + '</strong><br><span class="muted">' + escapeHTML(db.description || "") + '</span></td>' +
                            '  <td><code>' + escapeHTML(db.host + ":" + db.port + "/" + db.databaseName) + '</code></td>' +
                            '  <td>' + escapeHTML(db.username) + ' <span class="badge badge-neutral">' + escapeHTML(db.sslMode) + '</span></td>' +
                            '  <td id="db-test-status-' + escapeHTML(db.id) + '"><span class="badge badge-neutral">Belum diuji</span></td>' +
                            '  <td class="header-actions">' +
                            '    <button type="button" class="btn btn-outline btn-sm" data-action="test-db" data-id="' + escapeHTML(db.id) + '">Uji Koneksi</button>' +
                            '    <button type="button" class="btn btn-outline btn-sm" data-action="edit-db" data-id="' + escapeHTML(db.id) + '">Edit</button>' +
                            '    <button type="button" class="btn btn-danger btn-sm" data-action="delete-db" data-id="' + escapeHTML(db.id) + '">Hapus</button>' +
                            '  </td>' +
                            '</tr>'
                        );
                    }).join("");
                }
            }
            const grantSelect = document.getElementById("grant-db-select");
            if (grantSelect) {
                grantSelect.innerHTML = adminDatabases.length === 0
                    ? '<option value="">-- Buat koneksi database dulu --</option>'
                    : adminDatabases.map(function (db) {
                        return '<option value="' + escapeHTML(db.id) + '">' + escapeHTML(db.name + " (" + db.id + ")") + '</option>';
                    }).join("");
            }
        }

        if (dbTbody) {
            dbTbody.addEventListener("click", async function (e) {
                const btn = e.target.closest("button[data-action]");
                if (!btn) return;
                const action = btn.getAttribute("data-action");
                const id = btn.getAttribute("data-id");
                const found = adminDatabases.find(function (d) { return d.id === id; });

                if (action === "test-db") {
                    const cell = document.getElementById("db-test-status-" + id);
                    if (cell) cell.innerHTML = '<span class="badge badge-neutral">Menguji...</span>';
                    try {
                        const status = await apiFetch("/api/admin/databases/" + encodeURIComponent(id) + "/test", { method: "POST" });
                        if (cell) {
                            cell.innerHTML = '<span class="badge badge-success">Terhubung (' + escapeHTML(status.currentDatabase || "OK") + ')</span>';
                        }
                    } catch (err) {
                        if (cell) {
                            cell.innerHTML = '<span class="badge badge-danger" title="' + escapeHTML(err.message) + '">Gagal</span>';
                        }
                        showAlert(err.message, "error");
                    }
                } else if (action === "edit-db" && found) {
                    editingIDInput.value = found.id;
                    dbIDInput.value = found.id;
                    dbIDInput.disabled = true;
                    document.getElementById("db-name").value = found.name;
                    document.getElementById("db-host").value = found.host;
                    document.getElementById("db-port").value = found.port;
                    document.getElementById("db-database-name").value = found.databaseName;
                    document.getElementById("db-username").value = found.username;
                    document.getElementById("db-password").value = "";
                    document.getElementById("db-ssl-mode").value = found.sslMode;
                    document.getElementById("db-description").value = found.description || "";
                    if (dbSubmitBtn) dbSubmitBtn.textContent = "Perbarui Koneksi (" + found.id + ")";
                    if (dbPwdHint) dbPwdHint.textContent = "(kosongkan jika tidak ingin mengubah password)";
                } else if (action === "delete-db") {
                    try {
                        await apiFetch("/api/admin/databases/" + encodeURIComponent(id), { method: "DELETE" });
                        showAlert("Koneksi database " + id + " berhasil dihapus.", "success");
                        if (editingIDInput.value === id) resetDatabaseForm();
                        await loadAdminDatabases();
                        if (selectedGroupID) await loadGroupDetails(selectedGroupID, selectedGroupName);
                    } catch (err) {
                        showAlert(err.message, "error");
                    }
                }
            });
        }

        const dbForm = document.getElementById("db-connection-form");
        if (dbForm) {
            dbForm.addEventListener("submit", async function (e) {
                e.preventDefault();
                showAlert("");
                const editingID = editingIDInput.value.trim();
                const payload = {
                    id: dbIDInput.value.trim(),
                    name: document.getElementById("db-name").value.trim(),
                    host: document.getElementById("db-host").value.trim(),
                    port: parseInt(document.getElementById("db-port").value, 10) || 5432,
                    databaseName: document.getElementById("db-database-name").value.trim(),
                    username: document.getElementById("db-username").value.trim(),
                    password: document.getElementById("db-password").value,
                    sslMode: document.getElementById("db-ssl-mode").value,
                    description: document.getElementById("db-description").value.trim()
                };
                try {
                    if (editingID) {
                        await apiFetch("/api/admin/databases/" + encodeURIComponent(editingID), {
                            method: "PUT",
                            body: payload
                        });
                        showAlert("Koneksi database " + editingID + " berhasil diperbarui.", "success");
                    } else {
                        const created = await apiFetch("/api/admin/databases", {
                            method: "POST",
                            body: payload
                        });
                        showAlert("Koneksi database " + created.id + " berhasil ditambahkan.", "success");
                    }
                    resetDatabaseForm();
                    await loadAdminDatabases();
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        async function loadAdminUsers() {
            const data = await apiFetch("/api/admin/users");
            adminUsers = data.users || [];
            if (usersTbody) {
                usersTbody.innerHTML = adminUsers.map(function (u) {
                    return (
                        '<tr>' +
                        '  <td>' + escapeHTML(u.id) + '</td>' +
                        '  <td><strong>' + escapeHTML(u.username) + '</strong></td>' +
                        '  <td>' + (u.platformAdmin ? '<span class="badge badge-success">Platform Admin</span>' : '<span class="badge badge-neutral">Pengguna Biasa</span>') + '</td>' +
                        '  <td>' + (u.disabled ? '<span class="badge badge-danger">Nonaktif</span>' : '<span class="badge badge-success">Aktif</span>') + '</td>' +
                        '  <td class="header-actions">' +
                        '    <button type="button" class="btn btn-outline btn-sm" data-user-action="toggle-admin" data-id="' + u.id + '" data-admin="' + u.platformAdmin + '">' + (u.platformAdmin ? "Cabut Admin" : "Jadikan Admin") + '</button>' +
                        '    <button type="button" class="btn btn-outline btn-sm" data-user-action="toggle-disabled" data-id="' + u.id + '" data-disabled="' + u.disabled + '">' + (u.disabled ? "Aktifkan" : "Nonaktifkan") + '</button>' +
                        '  </td>' +
                        '</tr>'
                    );
                }).join("");
            }
            const memberSelect = document.getElementById("member-user-select");
            if (memberSelect) {
                memberSelect.innerHTML = adminUsers.map(function (u) {
                    return '<option value="' + u.id + '">' + escapeHTML(u.username) + ' (#' + u.id + ')</option>';
                }).join("");
            }
        }

        if (usersTbody) {
            usersTbody.addEventListener("click", async function (e) {
                const btn = e.target.closest("button[data-user-action]");
                if (!btn) return;
                const action = btn.getAttribute("data-user-action");
                const userID = btn.getAttribute("data-id");
                try {
                    if (action === "toggle-admin") {
                        const currentlyAdmin = btn.getAttribute("data-admin") === "true";
                        await apiFetch("/api/admin/users/" + userID + "/platform-admin", {
                            method: "PATCH",
                            body: { platformAdmin: !currentlyAdmin }
                        });
                    } else if (action === "toggle-disabled") {
                        const currentlyDisabled = btn.getAttribute("data-disabled") === "true";
                        await apiFetch("/api/admin/users/" + userID + "/disabled", {
                            method: "PATCH",
                            body: { disabled: !currentlyDisabled }
                        });
                    }
                    await loadAdminUsers();
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const createUserForm = document.getElementById("create-user-form");
        if (createUserForm) {
            createUserForm.addEventListener("submit", async function (e) {
                e.preventDefault();
                showAlert("");
                const username = document.getElementById("new-user-username").value.trim();
                const password = document.getElementById("new-user-password").value;
                try {
                    await apiFetch("/api/admin/users", {
                        method: "POST",
                        body: { username: username, password: password }
                    });
                    createUserForm.reset();
                    showAlert("Pengguna " + username + " berhasil dibuat.", "success");
                    await loadAdminUsers();
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        async function loadAdminGroups() {
            const data = await apiFetch("/api/admin/groups");
            adminGroups = data.groups || [];
            if (groupsTbody) {
                if (adminGroups.length === 0) {
                    groupsTbody.innerHTML = '<tr><td colspan="3" class="muted">Belum ada grup terdaftar.</td></tr>';
                } else {
                    groupsTbody.innerHTML = adminGroups.map(function (g) {
                        return (
                            '<tr>' +
                            '  <td>' + g.id + '</td>' +
                            '  <td><strong>' + escapeHTML(g.name) + '</strong></td>' +
                            '  <td class="header-actions">' +
                            '    <button type="button" class="btn btn-primary btn-sm" data-group-action="manage" data-id="' + g.id + '" data-name="' + escapeHTML(g.name) + '">Kelola Anggota &amp; Akses DB</button>' +
                            '    <button type="button" class="btn btn-danger btn-sm" data-group-action="delete" data-id="' + g.id + '">Hapus</button>' +
                            '  </td>' +
                            '</tr>'
                        );
                    }).join("");
                }
            }
        }

        async function loadGroupDetails(groupID, groupName) {
            selectedGroupID = groupID;
            selectedGroupName = groupName;
            const panel = document.getElementById("group-detail-panel");
            const heading = document.getElementById("selected-group-heading");
            if (panel) panel.hidden = false;
            if (heading) heading.textContent = "Kelola Grup: " + groupName + " (#" + groupID + ")";

            const [membersData, grantsData] = await Promise.all([
                apiFetch("/api/admin/groups/" + groupID + "/members"),
                apiFetch("/api/admin/groups/" + groupID + "/databases")
            ]);

            const membersList = document.getElementById("group-members-list");
            if (membersList) {
                const members = membersData.members || [];
                membersList.innerHTML = members.length === 0
                    ? '<li class="muted">Belum ada anggota dalam grup ini.</li>'
                    : members.map(function (m) {
                        return (
                            '<li>' +
                            '  <span><strong>' + escapeHTML(m.username) + '</strong> (#' + m.id + ')</span>' +
                            '  <button type="button" class="btn btn-outline btn-sm" data-remove-member="' + m.id + '">Keluarkan</button>' +
                            '</li>'
                        );
                    }).join("");
            }

            const grantsList = document.getElementById("group-grants-list");
            if (grantsList) {
                const grants = grantsData.databases || [];
                grantsList.innerHTML = grants.length === 0
                    ? '<li class="muted">Grup ini belum memiliki akses ke database mana pun.</li>'
                    : grants.map(function (g) {
                        const badgeClass = g.access === "view_edit" ? "badge-success" : "badge-warning";
                        return (
                            '<li>' +
                            '  <span><code>' + escapeHTML(g.databaseId) + '</code> — <span class="badge ' + badgeClass + '">' + escapeHTML(formatAccessLabel(g.access)) + '</span></span>' +
                            '  <button type="button" class="btn btn-outline btn-sm" data-revoke-db="' + escapeHTML(g.databaseId) + '">Cabut Akses</button>' +
                            '</li>'
                        );
                    }).join("");
            }
        }

        if (groupsTbody) {
            groupsTbody.addEventListener("click", async function (e) {
                const btn = e.target.closest("button[data-group-action]");
                if (!btn) return;
                const action = btn.getAttribute("data-group-action");
                const groupID = btn.getAttribute("data-id");
                const groupName = btn.getAttribute("data-name");

                if (action === "manage") {
                    try {
                        await loadGroupDetails(groupID, groupName);
                    } catch (err) {
                        showAlert(err.message, "error");
                    }
                } else if (action === "delete") {
                    try {
                        await apiFetch("/api/admin/groups/" + groupID, { method: "DELETE" });
                        if (String(selectedGroupID) === String(groupID)) {
                            selectedGroupID = null;
                            document.getElementById("group-detail-panel").hidden = true;
                        }
                        await loadAdminGroups();
                    } catch (err) {
                        showAlert(err.message, "error");
                    }
                }
            });
        }

        const createGroupForm = document.getElementById("create-group-form");
        if (createGroupForm) {
            createGroupForm.addEventListener("submit", async function (e) {
                e.preventDefault();
                showAlert("");
                const name = document.getElementById("new-group-name").value.trim();
                try {
                    const group = await apiFetch("/api/admin/groups", {
                        method: "POST",
                        body: { name: name }
                    });
                    createGroupForm.reset();
                    await loadAdminGroups();
                    await loadGroupDetails(group.id, group.name);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const addMemberForm = document.getElementById("add-member-form");
        if (addMemberForm) {
            addMemberForm.addEventListener("submit", async function (e) {
                e.preventDefault();
                if (!selectedGroupID) return;
                const userID = document.getElementById("member-user-select").value;
                try {
                    await apiFetch("/api/admin/groups/" + selectedGroupID + "/members/" + userID, { method: "PUT" });
                    await loadGroupDetails(selectedGroupID, selectedGroupName);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const membersList = document.getElementById("group-members-list");
        if (membersList) {
            membersList.addEventListener("click", async function (e) {
                const btn = e.target.closest("button[data-remove-member]");
                if (!btn || !selectedGroupID) return;
                const userID = btn.getAttribute("data-remove-member");
                try {
                    await apiFetch("/api/admin/groups/" + selectedGroupID + "/members/" + userID, { method: "DELETE" });
                    await loadGroupDetails(selectedGroupID, selectedGroupName);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const grantDBForm = document.getElementById("grant-db-form");
        if (grantDBForm) {
            grantDBForm.addEventListener("submit", async function (e) {
                e.preventDefault();
                if (!selectedGroupID) return;
                const dbID = document.getElementById("grant-db-select").value;
                const access = document.getElementById("grant-access-select").value;
                if (!dbID) {
                    showAlert("Pilih koneksi database terlebih dahulu.", "error");
                    return;
                }
                try {
                    await apiFetch("/api/admin/groups/" + selectedGroupID + "/databases/" + encodeURIComponent(dbID) + "/access", {
                        method: "PUT",
                        body: { access: access }
                    });
                    showAlert("Izin akses grup ke database " + dbID + " berhasil disimpan.", "success");
                    await loadGroupDetails(selectedGroupID, selectedGroupName);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        const grantsList = document.getElementById("group-grants-list");
        if (grantsList) {
            grantsList.addEventListener("click", async function (e) {
                const btn = e.target.closest("button[data-revoke-db]");
                if (!btn || !selectedGroupID) return;
                const dbID = btn.getAttribute("data-revoke-db");
                try {
                    await apiFetch("/api/admin/groups/" + selectedGroupID + "/databases/" + encodeURIComponent(dbID) + "/access", {
                        method: "DELETE"
                    });
                    await loadGroupDetails(selectedGroupID, selectedGroupName);
                } catch (err) {
                    showAlert(err.message, "error");
                }
            });
        }

        async function refreshAllAdminData() {
            try {
                await Promise.all([loadAdminDatabases(), loadAdminUsers(), loadAdminGroups()]);
                if (selectedGroupID) {
                    await loadGroupDetails(selectedGroupID, selectedGroupName);
                }
            } catch (err) {
                showAlert(err.message, "error");
            }
        }

        const refreshAllBtn = document.getElementById("admin-refresh-all-btn");
        if (refreshAllBtn) {
            refreshAllBtn.addEventListener("click", refreshAllAdminData);
        }
        await refreshAllAdminData();
    }

    document.addEventListener("DOMContentLoaded", function () {
        bindLogout();
        const page = document.body.getAttribute("data-page");
        const databaseID = document.body.getAttribute("data-database-id");
        if (page === "login") {
            initLoginPage();
        } else if (page === "workspace") {
            initWorkspacePage();
        } else if (page === "database") {
            initDatabasePage(databaseID);
        } else if (page === "admin") {
            initAdminPage();
        }
    });
})();
