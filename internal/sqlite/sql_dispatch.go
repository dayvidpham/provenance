package sqlite

import (
	"errors"
	"fmt"

	zs "zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var errUnknownSQLStatement = errors.New("sqlite: unknown sealed SQL statement")

type sqlStatement uint16

const (
	sqlStatement001 sqlStatement = iota + 1
	sqlStatement002
	sqlStatement003
	sqlStatement004
	sqlStatement005
	sqlStatement006
	sqlStatement007
	sqlStatement008
	sqlStatement009
	sqlStatement010
	sqlStatement011
	sqlStatement012
	sqlStatement013
	sqlStatement014
	sqlStatement015
	sqlStatement016
	sqlStatement017
	sqlStatement018
	sqlStatement019
	sqlStatement020
	sqlStatement021
	sqlStatement022
	sqlStatement023
	sqlStatement024
	sqlStatement025
	sqlStatement026
	sqlStatement027
	sqlStatement028
	sqlStatement029
	sqlStatement030
	sqlStatement031
	sqlStatement032
	sqlStatement033
	sqlStatement034
	sqlStatement035
	sqlStatement036
	sqlStatement037
	sqlStatement038
	sqlStatement039
	sqlStatement040
	sqlStatement041
	sqlStatement042
	sqlStatement043
	sqlStatement044
	sqlStatement045
	sqlStatement046
	sqlStatement047
	sqlStatement048
	sqlStatement049
	sqlStatement050
	sqlStatement051
	sqlStatement052
	sqlStatement053
	sqlStatement054
	sqlStatement055
	sqlStatement056
	sqlStatement057
	sqlStatement058
	sqlStatement059
	sqlStatement060
	sqlStatement061
	sqlStatement062
	sqlStatement063
	sqlStatement064
	sqlStatement065
	sqlStatement066
	sqlStatement067
	sqlStatement068
	sqlStatement069
	sqlStatement070
	sqlStatement071
	sqlStatement072
	sqlStatement073
	sqlStatement074
	sqlStatement075
	sqlStatement076
	sqlStatement077
	sqlStatement078
	sqlStatement079
	sqlStatement080
	sqlStatement081
	sqlStatement082
	sqlStatement083
	sqlStatement084
	sqlStatement085
	sqlStatement086
	sqlStatement087
	sqlStatement088
	sqlStatement089
	sqlStatement090
	sqlStatement091
	sqlStatement092
	sqlStatement093
	sqlStatement094
	sqlStatement095
	sqlStatement096
	sqlStatement097
	sqlStatement098
	sqlStatement099
	sqlStatement100
	sqlStatement101
	sqlStatement102
	sqlStatement103
	sqlStatement104
	sqlStatement105
	sqlStatement106
	sqlStatement107
	sqlStatement108
	sqlStatement109
	sqlStatement110
	sqlStatement111
	sqlStatement112
	sqlStatement113
	sqlStatement114
	sqlStatement115
	sqlStatement116
	sqlStatement117
	sqlStatement118
	sqlStatement119
	sqlStatement120
	sqlStatement121
	sqlStatement122
	sqlStatement123
	sqlStatement124
	sqlStatement125
	sqlStatement126
	sqlStatement127
	sqlStatement128
	sqlStatement129
	sqlStatement130
	sqlStatement131
	sqlStatement132
	sqlStatement133
	sqlStatement134
	sqlStatement135
	sqlStatement136
	sqlStatement137
	sqlStatement138
	sqlStatement139
	sqlStatement140
	sqlStatement141
	sqlStatement142
	sqlStatement143
	sqlStatement144
	sqlStatement145
	sqlStatement146
	sqlStatement147
	sqlStatement148
	sqlStatement149
	sqlStatement150
	sqlStatement151
	sqlStatement152
	sqlStatement153
	sqlStatement154
	sqlStatement155
	sqlStatement156
	sqlStatement157
	sqlStatement158
	sqlStatement159
	sqlStatement160
	sqlStatement161
	sqlStatement162
	sqlStatement163
	sqlStatement164
	sqlStatement165
	sqlStatement166
	sqlStatement167
	sqlStatement168
	sqlStatement169
	sqlStatement170
	sqlStatement171
	sqlStatement172
	sqlStatement173
	sqlStatement174
	sqlStatement175
	sqlStatement176
	sqlStatement177
	sqlStatement178
	sqlStatement179
	sqlStatement180
	sqlStatement181
	sqlStatement182
	sqlStatement183
	sqlStatement184
	sqlStatement185
	sqlStatement186
	sqlStatement187
	sqlStatement188
	sqlStatement189
	sqlStatement190
	sqlStatement191
	sqlStatement192
	sqlStatement193
	sqlStatement194
	sqlStatement195
	sqlStatement196
	sqlStatement197
	sqlStatement198
	sqlStatement199
	sqlStatement200
	sqlStatement201
	sqlStatement202
	sqlStatement203
	sqlStatement204
	sqlStatement205
	sqlStatement206
	sqlStatement207
	sqlStatement208
	sqlStatement209
	sqlStatement210
	sqlStatement211
	sqlStatement212
	sqlStatement213
	sqlStatement214
	sqlStatement215
	sqlStatement216
	sqlStatement217
	sqlStatement218
	sqlStatement219
	sqlStatement220
	sqlStatement221
	sqlStatement222
	sqlStatement223
	sqlStatement224
	sqlStatement225
	sqlStatement226
	sqlStatement227
	sqlStatement228
	sqlStatement229
	sqlStatement230
	sqlStatement231
	sqlStatement232
	sqlStatement233
	sqlStatement234
	sqlStatement235
	sqlStatement236
	sqlStatement237
	sqlStatement238
	sqlStatement239
	sqlStatement240
	sqlStatement241
	sqlStatement242
	sqlStatement243
	sqlStatement244
	sqlStatement245
	sqlStatement246
	sqlStatement247
	sqlStatement248
	sqlStatement249
	sqlStatement250
	sqlStatement251
	sqlStatement252
	sqlStatement253
	sqlStatement254
	sqlStatement255
	sqlStatement256
	sqlStatement257
	sqlStatement258
	sqlStatement259
	sqlStatement260
	sqlStatement261
	sqlStatement262
	sqlStatement263
	sqlStatement264
	sqlStatement265
	sqlStatement266
	sqlStatement267
	sqlStatement268
	sqlStatement269
	sqlStatement270
	sqlStatement271
	sqlStatement272
	sqlStatement273
	sqlStatement274
	sqlStatement275
	sqlStatement276
	sqlStatement277
	sqlStatement278
	sqlStatement279
	sqlStatement280
	sqlStatement281
	sqlStatement282
	sqlStatement283
	sqlStatement284
	sqlStatement285
	sqlStatement286
	sqlStatement287
	sqlStatement288
	sqlStatement289
	sqlStatement290
	sqlStatement291
	sqlStatement292
	sqlStatement293
	sqlStatement294
	sqlStatement295
	sqlStatement296
	sqlStatement297
	sqlStatement298
	sqlStatement299
	sqlStatement300
	sqlStatement301
	sqlStatement302
	sqlStatement303
	sqlStatement304
	sqlStatement305
	sqlStatement306
	sqlStatement307
	sqlStatement308
	sqlStatement309
	sqlStatement310
	sqlStatement311
	sqlStatement312
	sqlStatement313
	sqlStatement314
	sqlStatement315
	sqlStatement316
	sqlStatement317
	sqlStatement318
	sqlStatement319
	sqlStatement320
	sqlStatement321
	sqlStatement322
	sqlStatement323
	sqlStatement324
	sqlStatement325
	sqlStatement326
	sqlStatement327
	sqlStatement328
	sqlStatement329
	sqlStatement330
	sqlStatement331
	sqlStatement332
	sqlStatement333
	sqlStatement334
	sqlStatement335
	sqlStatement336
	sqlStatement337
	sqlStatement338
	sqlStatement339
	sqlStatement340
	sqlStatement341
	sqlStatement342
	sqlStatement343
	sqlStatement344
	sqlStatement345
	sqlStatement346
	sqlStatement347
	sqlStatement348
	sqlStatement349
	sqlStatement350
	sqlStatement351
	sqlStatement352
	sqlStatement353
	sqlStatement354
	sqlStatement355
	sqlStatement356
	sqlStatement357
	sqlStatement358
	sqlStatement359
	sqlStatement360
	sqlStatement361
	sqlStatement362
	sqlStatement363
	sqlStatement364
	sqlStatement365
)

func executeStatement(conn *zs.Conn, statement sqlStatement, options *sqlitex.ExecOptions) error {
	switch statement {
	case sqlStatement001:
		return sqlitex.Execute(conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", options)
	case sqlStatement002:
		return sqlitex.Execute(conn, "INSERT INTO activities (id, agent_id, phase_id, stage_id, started_at, ended_at, notes)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)\n\t\t ON CONFLICT(id) DO NOTHING", options)
	case sqlStatement003:
		return sqlitex.Execute(conn, "SELECT id, agent_id, phase_id, stage_id, started_at, ended_at, notes\n\t\t FROM activities WHERE id = ?1", options)
	case sqlStatement004:
		return sqlitex.Execute(conn, "UPDATE activities SET ended_at = ?2 WHERE id = ?1", options)
	case sqlStatement005:
		return sqlitex.Execute(conn, "SELECT id,agent_id,phase_id,stage_id,started_at,ended_at,notes FROM activities WHERE (NOT ?1 OR agent_id=?2) ORDER BY started_at ASC", options)
	case sqlStatement006:
		return sqlitex.ExecuteTransient(conn, "BEGIN IMMEDIATE", options)
	case sqlStatement007:
		return sqlitex.ExecuteTransient(conn, "ROLLBACK", options)
	case sqlStatement008:
		return sqlitex.ExecuteTransient(conn, "COMMIT", options)
	case sqlStatement009:
		return sqlitex.Execute(conn, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement010:
		return sqlitex.Execute(conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", options)
	case sqlStatement011:
		return sqlitex.Execute(conn, "INSERT INTO agents_software (agent_id, name, version, source) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement012:
		return sqlitex.Execute(conn, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement013:
		return sqlitex.Execute(conn, "SELECT a.kind_id, s.name, s.version, s.source FROM agents a LEFT JOIN agents_software s ON s.agent_id = a.id WHERE a.id = ?1", options)
	case sqlStatement014:
		return sqlitex.Execute(conn, "SELECT actor_id, namespace, kind_id, name, metadata FROM fixed_actor_manifest_entries WHERE actor_id = ?1 OR (namespace = ?2 AND name = ?3)", options)
	case sqlStatement015:
		return sqlitex.Execute(conn, "INSERT INTO actor_namespace_claims (namespace, claimant_id, range_min, range_max, codec)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement016:
		return sqlitex.Execute(conn, "INSERT INTO fixed_actor_manifest_entries (actor_id, namespace, kind_id, name, metadata)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement017:
		return sqlitex.Execute(conn, "SELECT namespace, claimant_id, range_min, range_max, codec\n\t\t FROM actor_namespace_claims ORDER BY namespace ASC", options)
	case sqlStatement018:
		return sqlitex.Execute(conn, "SELECT namespace, claimant_id, range_min, range_max, codec\n\t\t FROM actor_namespace_claims WHERE namespace = ?1", options)
	case sqlStatement019:
		return sqlitex.Execute(conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", appendStaticSQLArgs(options, 0))
	case sqlStatement020:
		return sqlitex.Execute(conn, "INSERT INTO agents_human (agent_id, name, contact) VALUES (?1, ?2, ?3)", options)
	case sqlStatement021:
		return sqlitex.Execute(conn, "SELECT id FROM ml_models WHERE provider_id = (SELECT id FROM providers WHERE name = ?1) AND name = ?2", options)
	case sqlStatement022:
		return sqlitex.Execute(conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", appendStaticSQLArgs(options, 1))
	case sqlStatement023:
		return sqlitex.Execute(conn, "INSERT INTO agents_ml (agent_id, role_id, model_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement024:
		return sqlitex.Execute(conn, "INSERT INTO agents (id, kind_id) VALUES (?1, ?2)", appendStaticSQLArgs(options, 2))
	case sqlStatement025:
		return sqlitex.Execute(conn, "SELECT id, kind_id FROM agents WHERE id = ?1", options)
	case sqlStatement026:
		return sqlitex.Execute(conn, "SELECT a.kind_id, h.name, h.contact\n\t\t FROM agents a JOIN agents_human h ON a.id = h.agent_id\n\t\t WHERE a.id = ?1", options)
	case sqlStatement027:
		return sqlitex.Execute(conn, "SELECT a.kind_id, m.role_id, ml.id, p.name, ml.name\n\t\t FROM agents a\n\t\t JOIN agents_ml m ON a.id = m.agent_id\n\t\t JOIN ml_models ml ON m.model_id = ml.id\n\t\t JOIN providers p ON ml.provider_id = p.id\n\t\t WHERE a.id = ?1", options)
	case sqlStatement028:
		return sqlitex.Execute(conn, "SELECT a.kind_id, s.name, s.version, s.source\n\t\t FROM agents a JOIN agents_software s ON a.id = s.agent_id\n\t\t WHERE a.id = ?1", options)
	case sqlStatement029:
		return sqlitex.Execute(conn, "INSERT INTO comments (id, task_id, author_id, body, created_at) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement030:
		return sqlitex.Execute(conn, "SELECT id, task_id, author_id, body, created_at FROM comments WHERE id = ?1", options)
	case sqlStatement031:
		return sqlitex.Execute(conn, "SELECT id, task_id, author_id, body, created_at\n\t\t FROM comments WHERE task_id = ?1 ORDER BY created_at ASC", options)
	case sqlStatement032:
		return sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode=WAL", options)
	case sqlStatement033:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=ON", options)
	case sqlStatement034:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO statuses (id,name) VALUES (?1,?2)", options)
	case sqlStatement035:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO priorities (id,name) VALUES (?1,?2)", options)
	case sqlStatement036:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_types (id,name) VALUES (?1,?2)", options)
	case sqlStatement037:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edge_kinds (id,name) VALUES (?1,?2)", options)
	case sqlStatement038:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO agent_kinds (id,name) VALUES (?1,?2)", options)
	case sqlStatement039:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO providers (id,name) VALUES (?1,?2)", options)
	case sqlStatement040:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO roles (id,name) VALUES (?1,?2)", options)
	case sqlStatement041:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO phases (id,name) VALUES (?1,?2)", options)
	case sqlStatement042:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO stages (id,name) VALUES (?1,?2)", options)
	case sqlStatement043:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM ml_models", options)
	case sqlStatement044:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO ml_models (provider_id, name) VALUES ((SELECT id FROM providers WHERE name = ?1), ?2)", options)
	case sqlStatement045:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edges (source_id, target_id, kind_id, created_at) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement046:
		return sqlitex.Execute(conn, "DELETE FROM edges WHERE source_id = ?1 AND target_id = ?2 AND kind_id = ?3", options)
	case sqlStatement047:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id FROM edges WHERE source_id=?1 AND (NOT ?2 OR kind_id=?3) ORDER BY created_at ASC", options)
	case sqlStatement048:
		return sqlitex.Execute(conn, "SELECT source_id, target_id, kind_id FROM edges WHERE kind_id = ?1 ORDER BY created_at ASC", options)
	case sqlStatement049:
		return sqlitex.Execute(conn, "SELECT source_id, target_id FROM edges WHERE kind_id = ?1", options)
	case sqlStatement050:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_kinds (id, name) VALUES (?1, ?2)", options)
	case sqlStatement051:
		return sqlitex.Execute(conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case sqlStatement052:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload)\n\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement053:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_task_event_contexts\n\t\t\t\t(event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement054:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id)\n\t\t VALUES (?1, ?2, ?3)", options)
	case sqlStatement055:
		return sqlitex.Execute(conn, "UPDATE tasks SET last_journal_id = ?1 WHERE id = ?2", options)
	case sqlStatement056:
		return sqlitex.Execute(conn, "PRAGMA foreign_key_check", options)
	case sqlStatement057:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.produced_by_operation_journal_id IS NOT ?1 AND o.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, nil, 1))
	case sqlStatement058:
		return sqlitex.Execute(conn, "SELECT e.assignment_id FROM journal_authority_assignment_episodes e LEFT JOIN journal_authority_assignment_transitions t ON t.assignment_id=e.assignment_id WHERE t.journal_id IS ?1 LIMIT ?2", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement059:
		return sqlitex.Execute(conn, "SELECT id FROM tasks WHERE last_journal_id IS ?1 LIMIT ?2", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement060:
		return sqlitex.Execute(conn, "SELECT journal_id, produced_by_operation_journal_id IS NOT ?1\n\t\t FROM journal\n\t\t WHERE (produced_by_operation_journal_id IS NOT ?2 AND actor_id IS NOT ?3)\n\t\t    OR (produced_by_operation_journal_id IS ?4     AND actor_id IS ?5)\n\t\t LIMIT ?6", appendStaticSQLArgs(options, nil, nil, nil, nil, nil, 1))
	case sqlStatement061:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_operations s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement062:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_operations s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement063:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_task_events s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement064:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_task_events s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement065:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_authorities s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement066:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_authorities s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement067:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_decisions s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement068:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_decisions s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement069:
		return sqlitex.Execute(conn, "SELECT j.journal_id FROM journal j LEFT JOIN journal_evidence s ON s.journal_id=j.journal_id WHERE j.kind_id=?1 AND s.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement070:
		return sqlitex.Execute(conn, "SELECT s.journal_id FROM journal_evidence s JOIN journal j ON j.journal_id=s.journal_id WHERE j.kind_id<>?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement071:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_task_events b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement072:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement073:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement074:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_operations a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement075:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_authorities b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement076:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement077:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_task_events a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement078:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a JOIN journal_decisions b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement079:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement080:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_decisions a JOIN journal_evidence b ON a.journal_id=b.journal_id LIMIT ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement081:
		return sqlitex.Execute(conn, "SELECT ?3 FROM sqlite_master WHERE type=?1 AND name=?2", appendStaticSQLArgs(options, 1))
	case sqlStatement082:
		return sqlitex.Execute(conn, "SELECT COALESCE(MAX(journal_id), ?1) FROM journal", options)
	case sqlStatement083:
		return sqlitex.Execute(conn, "SELECT context_kind, context_identity FROM journal_task_event_contexts\n\t\t WHERE event_journal_id = ?1 ORDER BY context_kind, context_identity", options)
	case sqlStatement084:
		return sqlitex.Execute(conn, "SELECT task_id, actor_id, first_journal_id FROM task_attributions\n\t\t WHERE task_id = ?1 ORDER BY first_journal_id ASC", options)
	case sqlStatement085:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO labels (task_id, name) VALUES (?1, ?2)", options)
	case sqlStatement086:
		return sqlitex.Execute(conn, "DELETE FROM labels WHERE task_id = ?1 AND name = ?2", options)
	case sqlStatement087:
		return sqlitex.Execute(conn, "SELECT name FROM labels WHERE task_id = ?1 ORDER BY name ASC", options)
	case sqlStatement088:
		return sqlitex.Execute(conn, "INSERT INTO tasks\n\t\t\t(id, namespace, title, description, status_id, priority_id, type_id,\n\t\t\t phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14)", options)
	case sqlStatement089:
		return sqlitex.Execute(conn, "SELECT name FROM sqlite_master WHERE type=?1\n\t\t   AND (name = ?2 OR name LIKE ?3 ESCAPE ?4)\n\t\t ORDER BY name ASC", options)
	case sqlStatement090:
		return sqlitex.Execute(conn, "SELECT name FROM pragma_table_info(?1)", options)
	case sqlStatement091:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_operations WHERE operation_id LIKE ?1", options)
	case sqlStatement092:
		return sqlitex.Execute(conn, "SELECT j.recorded_at FROM journal_authority_assignment_transitions t\n\t\t JOIN journal j ON j.journal_id = t.journal_id\n\t\t WHERE t.assignment_id = ?1 AND t.transition_id = ?2", options)
	case sqlStatement093:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1", options)
	case sqlStatement094:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_events (journal_id, task_id, event_kind, payload) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement095:
		return sqlitex.Execute(conn, "INSERT INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement096:
		return sqlitex.Execute(conn, "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM shadow_edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	case sqlStatement097:
		return sqlitex.Execute(conn, "WITH RECURSIVE reach(node) AS (SELECT ?1 UNION SELECT e.target_id FROM edges e JOIN reach r ON e.source_id=r.node WHERE e.kind_id=?3) SELECT ?4 FROM reach WHERE node=?2 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	case sqlStatement098:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)", options)
	case sqlStatement099:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO edges (source_id,target_id,kind_id,created_at) VALUES (?1,?2,?3,?4)", options)
	case sqlStatement100:
		return sqlitex.Execute(conn, "DELETE FROM shadow_edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3", options)
	case sqlStatement101:
		return sqlitex.Execute(conn, "DELETE FROM edges WHERE source_id=?1 AND target_id=?2 AND kind_id=?3", options)
	case sqlStatement102:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_labels (task_id,name) VALUES (?1,?2)", options)
	case sqlStatement103:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO labels (task_id,name) VALUES (?1,?2)", options)
	case sqlStatement104:
		return sqlitex.Execute(conn, "DELETE FROM shadow_labels WHERE task_id=?1 AND name=?2", options)
	case sqlStatement105:
		return sqlitex.Execute(conn, "DELETE FROM labels WHERE task_id=?1 AND name=?2", options)
	case sqlStatement106:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)", options)
	case sqlStatement107:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO comments (id,task_id,author_id,body,created_at) VALUES (?1,?2,?3,?4,?5)", options)
	case sqlStatement108:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO authority_kinds (id, name) VALUES (?1, ?2)", options)
	case sqlStatement109:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO assignment_slots (id, name) VALUES (?1, ?2)", options)
	case sqlStatement110:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO assignment_transitions (id, name) VALUES (?1, ?2)", options)
	case sqlStatement111:
		return sqlitex.ExecuteTransient(conn, "PRAGMA table_info(journal_operations)", options)
	case sqlStatement112:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations ADD COLUMN mutation_encoding_version TEXT", options)
	case sqlStatement113:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations ADD COLUMN canonical_mutation BLOB", options)
	case sqlStatement114:
		return sqlitex.Execute(conn, "SELECT sql FROM sqlite_master WHERE type=?1 AND name=?2", options)
	case sqlStatement115:
		return sqlitex.ExecuteTransient(conn, "DROP TRIGGER IF EXISTS journal_operations_canonical_insert", options)
	case sqlStatement116:
		return sqlitex.ExecuteTransient(conn, "DROP TRIGGER IF EXISTS journal_operations_canonical_update", options)
	case sqlStatement117:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_operations_generic (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (length(mutation_encoding_version)>0 AND length(canonical_mutation)>0))) STRICT", options)
	case sqlStatement118:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations_generic SELECT * FROM journal_operations", options)
	case sqlStatement119:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE journal_operations", options)
	case sqlStatement120:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations_generic RENAME TO journal_operations", options)
	case sqlStatement121:
		return sqlitex.Execute(conn, "SELECT operation_id,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE (mutation_encoding_version IS ?1) != (canonical_mutation IS ?2) OR (mutation_encoding_version IS NOT ?3 AND (NOT length(mutation_encoding_version) OR NOT length(canonical_mutation))) LIMIT ?4", appendStaticSQLArgs(options, nil, nil, nil, 1))
	case sqlStatement122:
		return sqlitex.Execute(conn, "SELECT operation_id FROM journal_operations WHERE canonical_mutation IS NOT ?2 AND length(canonical_mutation)>?1 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement123:
		return sqlitex.Execute(conn, "SELECT operation_id,mutation_encoding_version,canonical_mutation FROM journal_operations WHERE canonical_mutation IS NOT ?1", appendStaticSQLArgs(options, nil))
	case sqlStatement124:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_check", options)
	case sqlStatement125:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_key_list(journal)", options)
	case sqlStatement126:
		return sqlitex.Execute(conn, "SELECT ?1 FROM journal LIMIT ?2", appendStaticSQLArgs(options, 1, 1))
	case sqlStatement127:
		return sqlitex.Execute(conn, "SELECT ?2 FROM tasks WHERE id = ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement128:
		return sqlitex.Execute(conn, "INSERT INTO tasks\n\t\t\t(id, namespace, title, description, status_id, priority_id, type_id,\n\t\t\t phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?13, ?9, ?10, ?11, ?14, ?9, ?12)", appendStaticSQLArgs(options, nil, nil))
	case sqlStatement129:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO journal_task_event_contexts (event_journal_id, context_kind, context_identity, attached_by_journal_id)\n\t\t\t VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement130:
		return sqlitex.Execute(conn, "UPDATE tasks SET\n\t\tupdated_at=?1,\n\t\ttitle=CASE WHEN ?2 THEN ?3 ELSE title END,\n\t\tdescription=CASE WHEN ?4 THEN ?5 ELSE description END,\n\t\tpriority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END,\n\t\tphase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,\n\t\tnotes=CASE WHEN ?10 THEN ?11 ELSE notes END,\n\t\tclose_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END\n\t\tWHERE id=?14", options)
	case sqlStatement131:
		return sqlitex.Execute(conn, "INSERT INTO journal_authorities (journal_id, authority_kind_id, operation_authority_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement132:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_bootstraps (journal_id, label) VALUES (?1, ?2)", options)
	case sqlStatement133:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6)", options)
	case sqlStatement134:
		return sqlitex.Execute(conn, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement135:
		return sqlitex.Execute(conn, "INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?3, ?4, ?5)", options)
	case sqlStatement136:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=OFF", options)
	case sqlStatement137:
		return sqlitex.Execute(conn, "INSERT INTO journal_decisions (journal_id, decision_kind, task_id, payload) VALUES (?1, ?2, ?4, ?3)", appendStaticSQLArgs(options, nil))
	case sqlStatement138:
		return sqlitex.Execute(conn, "INSERT INTO journal_evidence (journal_id, evidence_kind, task_id, content_digest, payload) VALUES (?1, ?2, ?5, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case sqlStatement139:
		return sqlitex.ExecuteTransient(conn, "PRAGMA ignore_check_constraints=ON", options)
	case sqlStatement140:
		return sqlitex.ExecuteTransient(conn, "PRAGMA ignore_check_constraints=OFF", options)
	case sqlStatement141:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest)\n\t\t VALUES (?1, ?2, ?5, ?3, ?4)", appendStaticSQLArgs(options, nil))
	case sqlStatement142:
		return sqlitex.Execute(conn, "INSERT INTO journal (kind_id, actor_id, recorded_at, produced_by_operation_journal_id) VALUES (?1, ?2, ?3, ?4)", options)
	case sqlStatement143:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", appendStaticSQLArgs(options, nil))
	case sqlStatement144:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_transitions (journal_id, assignment_id, transition_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement145:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_task_events ADD COLUMN unreviewed TEXT", options)
	case sqlStatement146:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_task_events DROP COLUMN payload", options)
	case sqlStatement147:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE journal", options)
	case sqlStatement148:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_unreviewed (journal_id INTEGER PRIMARY KEY) STRICT", options)
	case sqlStatement149:
		return sqlitex.Execute(conn, "UPDATE tasks SET owner_id=?1 WHERE id=?2", options)
	case sqlStatement150:
		return sqlitex.Execute(conn, "UPDATE tasks SET status_id=?1 WHERE id=?2", options)
	case sqlStatement151:
		return sqlitex.Execute(conn, "UPDATE tasks SET last_journal_id=?1 WHERE id=?2", options)
	case sqlStatement152:
		return sqlitex.Execute(conn, "UPDATE comments SET body = ?1 WHERE id = ?2", options)
	case sqlStatement153:
		return sqlitex.Execute(conn, "INSERT OR REPLACE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement154:
		return sqlitex.Execute(conn, "UPDATE journal_authority_assignment_episodes SET parent_assignment_id = ?1 WHERE assignment_id = ?2", options)
	case sqlStatement155:
		return sqlitex.Execute(conn, "INSERT INTO journal_authority_assignment_episodes (assignment_id, task_id, slot_id, actor_id, predecessor_assignment_id, parent_assignment_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?6, ?5)", appendStaticSQLArgs(options, nil))
	case sqlStatement156:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations\n\t\t (journal_id, operation_id, authority_journal_id, command_digest, mutation_digest, mutation_encoding_version, canonical_mutation)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)", options)
	case sqlStatement157:
		return sqlitex.Execute(conn, "INSERT INTO journal_operation_result_slots (journal_id, result_slot_id, produced_journal_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement158:
		return sqlitex.Execute(conn, "SELECT produced_by_operation_journal_id FROM journal WHERE journal_id = ?1", options)
	case sqlStatement159:
		return sqlitex.Execute(conn, "SELECT e.actor_id FROM journal_authority_assignment_episodes e\n\t\t JOIN journal_authority_assignment_transitions started\n\t\t   ON started.assignment_id = e.assignment_id AND started.transition_id = ?2\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?3\n\t\t   AND NOT EXISTS (SELECT ?5 FROM journal_authority_assignment_transitions ended\n\t\t                   WHERE ended.assignment_id = e.assignment_id AND ended.transition_id = ?4)\n\t\t ORDER BY started.journal_id DESC LIMIT ?6", appendStaticSQLArgs(options, 1, 1))
	case sqlStatement160:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO shadow_task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement161:
		return sqlitex.Execute(conn, "INSERT OR IGNORE INTO task_attributions (task_id, actor_id, first_journal_id) VALUES (?1, ?2, ?3)", options)
	case sqlStatement162:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET last_journal_id = ?1 WHERE id = ?2", options)
	case sqlStatement163:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3", options)
	case sqlStatement164:
		return sqlitex.Execute(conn, "UPDATE tasks SET owner_id = ?1, last_journal_id = ?2 WHERE id = ?3", options)
	case sqlStatement165:
		return sqlitex.Execute(conn, "SELECT ?2 FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement166:
		return sqlitex.Execute(conn, "SELECT ?3 FROM journal_authority_assignment_transitions WHERE assignment_id = ?1 AND transition_id = ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement167:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case sqlStatement168:
		return sqlitex.Execute(conn, "SELECT parent_assignment_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case sqlStatement169:
		return sqlitex.Execute(conn, "SELECT ?4 FROM journal_authority_assignment_transitions\n\t\t WHERE assignment_id = ?1 AND transition_id = ?2 AND journal_id < ?3 LIMIT ?5", appendStaticSQLArgs(options, 1, 1))
	case sqlStatement170:
		return sqlitex.Execute(conn, "SELECT ?5 FROM journal_authority_assignment_episodes e\n\t\t WHERE e.task_id = ?1 AND e.slot_id = ?2\n\t\t   AND EXISTS (SELECT ?6 FROM journal_authority_assignment_transitions s WHERE s.assignment_id = e.assignment_id AND s.transition_id = ?3)\n\t\t   AND NOT EXISTS (SELECT ?7 FROM journal_authority_assignment_transitions x WHERE x.assignment_id = e.assignment_id AND x.transition_id = ?4)\n\t\t LIMIT ?8", appendStaticSQLArgs(options, 1, 1, 1, 1))
	case sqlStatement171:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_operations", options)
	case sqlStatement172:
		return sqlitex.Execute(conn, "SELECT ?2 FROM journal_authorities WHERE journal_id = ?1", appendStaticSQLArgs(options, 1))
	case sqlStatement173:
		return sqlitex.Execute(conn, "SELECT authority_kind_id FROM journal_authorities WHERE journal_id = ?1", options)
	case sqlStatement174:
		return sqlitex.Execute(conn, "SELECT assignment_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case sqlStatement175:
		return sqlitex.Execute(conn, "SELECT assignment_id FROM journal_authority_assignment_episodes WHERE task_id = ?1", options)
	case sqlStatement176:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes", options)
	case sqlStatement177:
		return sqlitex.Execute(conn, "SELECT journal_id, authority_journal_id, command_digest, mutation_digest,\n\t\t        mutation_encoding_version, canonical_mutation\n\t\t FROM journal_operations WHERE operation_id = ?1", options)
	case sqlStatement178:
		return sqlitex.Execute(conn, "SELECT actor_id FROM journal WHERE journal_id = ?1", options)
	case sqlStatement179:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id=?1 ORDER BY journal_id", options)
	case sqlStatement180:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal WHERE produced_by_operation_journal_id = ?1 AND kind_id = ?2 ORDER BY journal_id ASC", options)
	case sqlStatement181:
		return sqlitex.Execute(conn, "SELECT s.result_slot_id, s.produced_journal_id, j.kind_id, te.task_id\n\t\t FROM journal_operation_result_slots s\n\t\t JOIN journal j ON j.journal_id = s.produced_journal_id\n\t\t LEFT JOIN journal_task_events te ON te.journal_id = s.produced_journal_id\n\t\t WHERE s.journal_id = ?1 ORDER BY s.result_slot_id ASC", options)
	case sqlStatement182:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authorities WHERE authority_kind_id = ?1", options)
	case sqlStatement183:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal_authority_assignment_episodes WHERE task_id = ?1 AND predecessor_assignment_id IS NOT ?2", appendStaticSQLArgs(options, nil))
	case sqlStatement184:
		return sqlitex.Execute(conn, "SELECT kind_id, effective_actor_id, recorded_at FROM journal_attributed WHERE journal_id = ?1", options)
	case sqlStatement185:
		return sqlitex.Execute(conn, "SELECT task_id, event_kind, payload FROM journal_task_events WHERE journal_id = ?1", options)
	case sqlStatement186:
		return sqlitex.Execute(conn, "SELECT assignment_id, transition_id FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case sqlStatement187:
		return sqlitex.Execute(conn, "SELECT task_id, actor_id, slot_id FROM journal_authority_assignment_episodes WHERE assignment_id = ?1", options)
	case sqlStatement188:
		return sqlitex.Execute(conn, "SELECT status_id FROM shadow_tasks WHERE id=?1", options)
	case sqlStatement189:
		return sqlitex.Execute(conn, "SELECT status_id FROM tasks WHERE id=?1", options)
	case sqlStatement190:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_evidence WHERE journal_id=?1", options)
	case sqlStatement191:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_decisions WHERE journal_id=?1", options)
	case sqlStatement192:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4", options)
	case sqlStatement193:
		return sqlitex.Execute(conn, "UPDATE tasks SET status_id=?1,closed_at=?2,last_journal_id=?3 WHERE id=?4", options)
	case sqlStatement194:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.authority_journal_id,?1,?2,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id", appendStaticSQLArgs(options, nil, nil))
	case sqlStatement195:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.authority_journal_id,o.mutation_encoding_version,o.canonical_mutation,o.mutation_digest,j.actor_id,j.recorded_at FROM journal_operations o JOIN journal j ON j.journal_id=o.journal_id ORDER BY o.journal_id", options)
	case sqlStatement196:
		return sqlitex.Execute(conn, "SELECT o.journal_id,?2,?3 FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1", appendStaticSQLArgs(options, nil, nil))
	case sqlStatement197:
		return sqlitex.Execute(conn, "SELECT o.journal_id,o.mutation_encoding_version,o.canonical_mutation FROM journal j JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE j.journal_id=?1", options)
	case sqlStatement198:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND (e.event_kind=?2 OR e.event_kind=?3))", appendStaticSQLArgs(options, nil, nil, nil, 1))
	case sqlStatement199:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks (id,namespace,title,description,owner_id,status_id,priority_id,type_id,phase_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT t.id,t.namespace,t.title,t.description,?4,?1,t.priority_id,t.type_id,t.phase_id,t.notes,t.created_at,t.updated_at,?5,t.close_reason,?6 FROM tasks t WHERE EXISTS (SELECT ?7 FROM journal_task_events e JOIN journal j ON j.journal_id=e.journal_id JOIN journal_operations o ON o.journal_id=j.produced_by_operation_journal_id WHERE e.task_id=t.id AND ((e.event_kind=?2 AND o.canonical_mutation IS ?8) OR e.event_kind=?3))", appendStaticSQLArgs(options, nil, nil, nil, 1, nil))
	case sqlStatement200:
		return sqlitex.Execute(conn, "SELECT result_slot_id,produced_journal_id FROM journal_operation_result_slots WHERE journal_id=?1", options)
	case sqlStatement201:
		return sqlitex.Execute(conn, "SELECT kind_id, recorded_at FROM journal WHERE journal_id=?1", options)
	case sqlStatement202:
		return sqlitex.Execute(conn, "SELECT a.operation_authority_id, b.label FROM journal_authorities a JOIN journal_authority_bootstraps b ON b.journal_id=a.journal_id WHERE a.journal_id=?1", options)
	case sqlStatement203:
		return sqlitex.Execute(conn, "SELECT e.assignment_id,e.task_id,e.slot_id,e.actor_id,e.predecessor_assignment_id,e.parent_assignment_id,t.transition_id FROM journal_authority_assignment_transitions t JOIN journal_authority_assignment_episodes e ON e.assignment_id=t.assignment_id WHERE t.journal_id=?1", options)
	case sqlStatement204:
		return sqlitex.Execute(conn, "SELECT assignment_id,transition_id FROM journal_authority_assignment_transitions WHERE journal_id=?1", options)
	case sqlStatement205:
		return sqlitex.Execute(conn, "SELECT decision_kind,task_id,payload FROM journal_decisions WHERE journal_id=?1", options)
	case sqlStatement206:
		return sqlitex.Execute(conn, "SELECT evidence_kind,task_id,hex(content_digest),payload FROM journal_evidence WHERE journal_id=?1", options)
	case sqlStatement207:
		return sqlitex.Execute(conn, "SELECT task_id,event_kind,payload FROM journal_task_events WHERE journal_id=?1", options)
	case sqlStatement208:
		return sqlitex.Execute(conn, "SELECT attached_by_journal_id FROM journal_task_event_contexts WHERE event_journal_id=?1 ORDER BY context_kind,context_identity", options)
	case sqlStatement209:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal ORDER BY journal_id ASC", options)
	case sqlStatement210:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal WHERE produced_by_operation_journal_id=?1 AND journal_id<=?2", options)
	case sqlStatement211:
		return sqlitex.Execute(conn, "INSERT INTO shadow_tasks\n\t\t (id, namespace, title, description, owner_id, status_id, priority_id, type_id,\n\t\t  phase_id, notes, created_at, updated_at, closed_at, close_reason, last_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?12, ?5, ?6, ?7, ?8, ?9, ?10, ?10, ?13, ?9, ?11)", appendStaticSQLArgs(options, nil, nil))
	case sqlStatement212:
		return sqlitex.Execute(conn, "UPDATE shadow_tasks SET\n\t\tupdated_at=?1,\n\t\ttitle=CASE WHEN ?2 THEN ?3 ELSE title END,\n\t\tdescription=CASE WHEN ?4 THEN ?5 ELSE description END,\n\t\tpriority_id=CASE WHEN ?6 THEN ?7 ELSE priority_id END,\n\t\tphase_id=CASE WHEN ?8 THEN ?9 ELSE phase_id END,\n\t\tnotes=CASE WHEN ?10 THEN ?11 ELSE notes END,\n\t\tclose_reason=CASE WHEN ?12 THEN ?13 ELSE close_reason END\n\t\tWHERE id=?14", options)
	case sqlStatement213:
		return sqlitex.Execute(conn, "SELECT task_id FROM journal_task_events\n\t\t UNION SELECT task_id FROM journal_authority_assignment_episodes\n\t\t UNION SELECT task_id FROM journal_decisions WHERE task_id IS NOT ?1\n\t\t UNION SELECT task_id FROM journal_evidence WHERE task_id IS NOT ?2", appendStaticSQLArgs(options, nil, nil))
	case sqlStatement214:
		return sqlitex.Execute(conn, "SELECT id,owner_id,status_id,last_journal_id FROM shadow_tasks", options)
	case sqlStatement215:
		return sqlitex.Execute(conn, "SELECT id,owner_id,status_id,last_journal_id FROM tasks", options)
	case sqlStatement216:
		return sqlitex.Execute(conn, "SELECT task_id,actor_id,first_journal_id FROM shadow_task_attributions", options)
	case sqlStatement217:
		return sqlitex.Execute(conn, "SELECT task_id,actor_id,first_journal_id FROM task_attributions", options)
	case sqlStatement218:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM shadow_tasks", options)
	case sqlStatement219:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks", options)
	case sqlStatement220:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id,created_at FROM shadow_edges", options)
	case sqlStatement221:
		return sqlitex.Execute(conn, "SELECT source_id,target_id,kind_id,created_at FROM edges", options)
	case sqlStatement222:
		return sqlitex.Execute(conn, "SELECT task_id,name FROM shadow_labels", options)
	case sqlStatement223:
		return sqlitex.Execute(conn, "SELECT task_id,name FROM labels", options)
	case sqlStatement224:
		return sqlitex.Execute(conn, "SELECT id,task_id,author_id,body,created_at FROM shadow_comments", options)
	case sqlStatement225:
		return sqlitex.Execute(conn, "SELECT id,task_id,author_id,body,created_at FROM comments", options)
	case sqlStatement226:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)) STRICT", options)
	case sqlStatement227:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '',last_journal_id INTEGER REFERENCES journal(journal_id)) STRICT", options)
	case sqlStatement228:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE tasks_watermark_rebuild (id TEXT PRIMARY KEY,namespace TEXT NOT NULL,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),priority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id),type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),phase_id INTEGER NOT NULL REFERENCES phases(id),owner_id TEXT REFERENCES agents(id),notes TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,closed_at INTEGER,close_reason TEXT NOT NULL DEFAULT '') STRICT", options)
	case sqlStatement229:
		return sqlitex.Execute(conn, "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason,last_journal_id FROM tasks", options)
	case sqlStatement230:
		return sqlitex.Execute(conn, "INSERT INTO tasks_watermark_rebuild (id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason) SELECT id,namespace,title,description,status_id,priority_id,type_id,phase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks", options)
	case sqlStatement231:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_namespace ON tasks (namespace)", options)
	case sqlStatement232:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status_id)", options)
	case sqlStatement233:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks (priority_id)", options)
	case sqlStatement234:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_type ON tasks (type_id)", options)
	case sqlStatement235:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_phase ON tasks (phase_id)", options)
	case sqlStatement236:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_owner ON tasks (owner_id)", options)
	case sqlStatement237:
		return sqlitex.Execute(conn, "PRAGMA table_info(tasks)", options)
	case sqlStatement238:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM tasks WHERE last_journal_id IS ?1", appendStaticSQLArgs(options, nil))
	case sqlStatement239:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE tasks", options)
	case sqlStatement240:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE tasks_watermark_rebuild RENAME TO tasks", options)
	case sqlStatement241:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE tasks ADD COLUMN last_journal_id INTEGER REFERENCES journal(journal_id)", options)
	case sqlStatement242:
		return sqlitex.Execute(conn, "DELETE FROM journal_operations WHERE journal_id=?1", options)
	case sqlStatement243:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_events WHERE journal_id=?1", options)
	case sqlStatement244:
		return sqlitex.Execute(conn, "DELETE FROM journal_authorities WHERE journal_id=?1", options)
	case sqlStatement245:
		return sqlitex.Execute(conn, "DELETE FROM journal_decisions WHERE journal_id=?1", options)
	case sqlStatement246:
		return sqlitex.Execute(conn, "DELETE FROM journal_evidence WHERE journal_id=?1", options)
	case sqlStatement247:
		return sqlitex.Execute(conn, "SELECT kind_id FROM journal WHERE journal_id = ?1", options)
	case sqlStatement248:
		return sqlitex.Execute(conn, "UPDATE journal SET kind_id = ?1 WHERE journal_id = ?2", options)
	case sqlStatement249:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM journal", options)
	case sqlStatement250:
		return sqlitex.Execute(conn, "SELECT journal_id FROM journal ORDER BY journal_id DESC LIMIT ?1", options)
	case sqlStatement251:
		return sqlitex.Execute(conn, "INSERT INTO journal (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t VALUES (?1, ?2, ?3, ?4, ?5)", appendStaticSQLArgs(options, 0, nil))
	case sqlStatement252:
		return sqlitex.Execute(conn, "DELETE FROM journal_operation_result_slots WHERE journal_id = ?1", options)
	case sqlStatement253:
		return sqlitex.Execute(conn, "DELETE FROM journal_authority_bootstraps WHERE journal_id = ?1", options)
	case sqlStatement254:
		return sqlitex.Execute(conn, "DELETE FROM journal_authority_assignment_transitions WHERE journal_id = ?1", options)
	case sqlStatement255:
		return sqlitex.Execute(conn, "DELETE FROM journal_authorities WHERE journal_id = ?1", options)
	case sqlStatement256:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_event_contexts WHERE event_journal_id = ?1", options)
	case sqlStatement257:
		return sqlitex.Execute(conn, "DELETE FROM journal_task_events WHERE journal_id = ?1", options)
	case sqlStatement258:
		return sqlitex.Execute(conn, "DELETE FROM journal_operations WHERE journal_id = ?1", options)
	case sqlStatement259:
		return sqlitex.Execute(conn, "DELETE FROM journal_decisions WHERE journal_id = ?1", options)
	case sqlStatement260:
		return sqlitex.Execute(conn, "DELETE FROM journal_evidence WHERE journal_id = ?1", options)
	case sqlStatement261:
		return sqlitex.Execute(conn, "DELETE FROM journal WHERE journal_id = ?1", options)
	case sqlStatement262:
		return sqlitex.Execute(conn, "SELECT journal_id, kind_id FROM journal ORDER BY journal_id ASC", options)
	case sqlStatement263:
		return sqlitex.Execute(conn, "SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload\n\t\t\tFROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id\n\t\t\tWHERE j.journal_id<=?1 AND j.journal_id>?3\n\t\t\t  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))\n\t\t\t  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))\n\t\t\t  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))\n\t\t\tORDER BY j.journal_id ASC LIMIT ?12", appendStaticSQLArgs(options, 1))
	case sqlStatement264:
		return sqlitex.Execute(conn, "SELECT j.journal_id,j.effective_actor_id,j.recorded_at,te.task_id,te.event_kind,te.payload\n\t\t\tFROM journal_attributed j JOIN journal_task_events te ON te.journal_id=j.journal_id\n\t\t\tWHERE j.journal_id<=?1 AND (j.recorded_at>?2 OR (j.recorded_at=?2 AND j.journal_id>?3))\n\t\t\t  AND (NOT ?4 OR te.task_id IN (SELECT value FROM json_each(?5)))\n\t\t\t  AND (NOT ?6 OR te.event_kind IN (SELECT value FROM json_each(?7)))\n\t\t\t  AND (NOT ?8 OR EXISTS (SELECT ?13 FROM journal_task_event_contexts ctx JOIN json_each(?9) f ON ctx.context_kind=json_extract(f.value,?10) AND ctx.context_identity=json_extract(f.value,?11) WHERE ctx.event_journal_id=te.journal_id))\n\t\t\tORDER BY j.recorded_at ASC,j.journal_id ASC LIMIT ?12", appendStaticSQLArgs(options, 1))
	case sqlStatement265:
		return sqlitex.Execute(conn, "SELECT id, namespace, title, description, status_id, priority_id, type_id,\n\t\t        phase_id, owner_id, notes, created_at, updated_at, closed_at, close_reason\n\t\t FROM tasks WHERE id = ?1", options)
	case sqlStatement266:
		return sqlitex.Execute(conn, "SELECT id,namespace,title,description,status_id,priority_id,type_id,\n\t\tphase_id,owner_id,notes,created_at,updated_at,closed_at,close_reason FROM tasks\n\t\tWHERE (NOT ?1 OR status_id=?2)\n\t\t  AND (NOT ?3 OR priority_id=?4)\n\t\t  AND (NOT ?5 OR type_id=?6)\n\t\t  AND (NOT ?7 OR phase_id=?8)\n\t\t  AND (NOT ?9 OR namespace=?10)\n\t\t  AND (NOT ?11 OR EXISTS (SELECT ?13 FROM labels l WHERE l.task_id=tasks.id AND l.name=?12))\n\t\tORDER BY created_at ASC", appendStaticSQLArgs(options, 1))
	case sqlStatement267:
		return sqlitex.Execute(conn, "SELECT COUNT(*) FROM tasks", options)
	case sqlStatement268:
		return sqlitex.Execute(conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND NOT EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", appendStaticSQLArgs(options, 1))
	case sqlStatement269:
		return sqlitex.Execute(conn, "\n\t\tSELECT t.id, t.namespace, t.title, t.description, t.status_id, t.priority_id,\n\t\t       t.type_id, t.phase_id, t.owner_id, t.notes, t.created_at, t.updated_at,\n\t\t       t.closed_at, t.close_reason\n\t\tFROM tasks t\n\t\tWHERE t.status_id != ?1\n\t\tAND EXISTS (\n\t\t\tSELECT ?3 FROM edges e\n\t\t\tJOIN tasks blocker ON e.target_id = blocker.id\n\t\t\tWHERE e.source_id = t.id AND e.kind_id = ?2 AND blocker.status_id != ?1\n\t\t)\n\t\tORDER BY t.priority_id ASC, t.created_at ASC", appendStaticSQLArgs(options, 1))
	case sqlStatement270:
		return sqlitex.ExecuteTransient(conn, "PRAGMA busy_timeout=5000;", options)
	case sqlStatement271:
		return sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=OFF;", options)
	case sqlStatement272:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS statuses (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement273:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS priorities (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement274:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS task_types (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement275:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS edge_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement276:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agent_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement277:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS providers (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement278:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS roles (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement279:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS phases (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement280:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS stages (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement281:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS ml_models (\n\t\t\tid          INTEGER PRIMARY KEY,\n\t\t\tprovider_id INTEGER NOT NULL REFERENCES providers(id),\n\t\t\tname        TEXT NOT NULL,\n\t\t\tUNIQUE (provider_id, name)\n\t\t) STRICT", options)
	case sqlStatement282:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents (\n\t\t\tid      TEXT PRIMARY KEY,\n\t\t\tkind_id INTEGER NOT NULL REFERENCES agent_kinds(id)\n\t\t) STRICT", options)
	case sqlStatement283:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_human (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tname     TEXT NOT NULL,\n\t\t\tcontact  TEXT NOT NULL DEFAULT ''\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement284:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_ml (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\trole_id  INTEGER NOT NULL REFERENCES roles(id),\n\t\t\tmodel_id INTEGER NOT NULL REFERENCES ml_models(id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement285:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS agents_software (\n\t\t\tagent_id TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tname     TEXT NOT NULL,\n\t\t\tversion  TEXT NOT NULL DEFAULT '',\n\t\t\tsource   TEXT NOT NULL DEFAULT ''\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement286:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS tasks (\n\t\t\tid TEXT PRIMARY KEY, namespace TEXT NOT NULL, title TEXT NOT NULL,\n\t\t\tdescription TEXT NOT NULL DEFAULT '', status_id INTEGER NOT NULL DEFAULT 0 REFERENCES statuses(id),\n\t\t\tpriority_id INTEGER NOT NULL DEFAULT 2 REFERENCES priorities(id), type_id INTEGER NOT NULL DEFAULT 2 REFERENCES task_types(id),\n\t\t\tphase_id INTEGER NOT NULL REFERENCES phases(id), owner_id TEXT REFERENCES agents(id), notes TEXT NOT NULL DEFAULT '',\n\t\t\tcreated_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, closed_at INTEGER, close_reason TEXT NOT NULL DEFAULT '',\n\t\t\tlast_journal_id INTEGER NOT NULL REFERENCES journal(journal_id)\n\t\t) STRICT", options)
	case sqlStatement287:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_status    ON tasks (status_id)", options)
	case sqlStatement288:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_priority  ON tasks (priority_id)", options)
	case sqlStatement289:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_type      ON tasks (type_id)", options)
	case sqlStatement290:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_phase     ON tasks (phase_id)", options)
	case sqlStatement291:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_tasks_owner     ON tasks (owner_id)", options)
	case sqlStatement292:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS edges (\n\t\t\tsource_id  TEXT NOT NULL REFERENCES tasks(id),\n\t\t\ttarget_id  TEXT NOT NULL,\n\t\t\tkind_id    INTEGER NOT NULL REFERENCES edge_kinds(id),\n\t\t\tcreated_at INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (source_id, target_id, kind_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement293:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_source ON edges (source_id)", options)
	case sqlStatement294:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_target ON edges (target_id)", options)
	case sqlStatement295:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_edges_kind   ON edges (kind_id)", options)
	case sqlStatement296:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS activities (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\tagent_id   TEXT NOT NULL REFERENCES agents(id),\n\t\t\tphase_id   INTEGER NOT NULL REFERENCES phases(id),\n\t\t\tstage_id   INTEGER NOT NULL REFERENCES stages(id),\n\t\t\tstarted_at INTEGER NOT NULL,\n\t\t\tended_at   INTEGER,\n\t\t\tnotes      TEXT NOT NULL DEFAULT ''\n\t\t) STRICT", options)
	case sqlStatement297:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_activities_agent ON activities (agent_id)", options)
	case sqlStatement298:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_activities_phase ON activities (phase_id)", options)
	case sqlStatement299:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS labels (\n\t\t\ttask_id TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tname    TEXT NOT NULL,\n\t\t\tPRIMARY KEY (task_id, name)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement300:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_labels_name ON labels (name)", options)
	case sqlStatement301:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS comments (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\ttask_id    TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tauthor_id  TEXT NOT NULL REFERENCES agents(id),\n\t\t\tbody       TEXT NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL\n\t\t) STRICT", options)
	case sqlStatement302:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_comments_task   ON comments (task_id)", options)
	case sqlStatement303:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_comments_author ON comments (author_id)", options)
	case sqlStatement304:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_kinds (\n\t\t\tid   INTEGER PRIMARY KEY,\n\t\t\tname TEXT NOT NULL UNIQUE\n\t\t) STRICT", options)
	case sqlStatement305:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal (\n\t\t\tjournal_id  INTEGER PRIMARY KEY AUTOINCREMENT,\n\t\t\tkind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),\n\t\t\tactor_id    TEXT REFERENCES agents(id),\n\t\t\trecorded_at INTEGER NOT NULL,\n\t\t\t-- The producing operation (§2.1, §4.6). NULL at the journal-base layer;\n\t\t\t-- the operations slice (dayvidpham/provenance#5) adds the FK to\n\t\t\t-- journal_operations(journal_id) when that subtype table lands.\n\t\t\tproduced_by_operation_journal_id INTEGER,\n\t\t\tCHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL))\n\t\t) STRICT", options)
	case sqlStatement306:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_kind  ON journal (kind_id)", options)
	case sqlStatement307:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_actor ON journal (actor_id)", options)
	case sqlStatement308:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_recorded_at ON journal (recorded_at, journal_id)", options)
	case sqlStatement309:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_task_events (\n\t\t\tjournal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\ttask_id    TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tevent_kind TEXT NOT NULL,\n\t\t\tpayload    TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case sqlStatement310:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_task_events_task ON journal_task_events (task_id)", options)
	case sqlStatement311:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_task_event_contexts (\n\t\t\tevent_journal_id       INTEGER NOT NULL REFERENCES journal_task_events(journal_id),\n\t\t\tcontext_kind           TEXT NOT NULL,\n\t\t\tcontext_identity       TEXT NOT NULL,\n\t\t\tattached_by_journal_id INTEGER NOT NULL REFERENCES journal_task_events(journal_id),\n\t\t\tPRIMARY KEY (event_journal_id, context_kind, context_identity)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement312:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS actor_namespace_claims (\n\t\t\tnamespace   TEXT PRIMARY KEY,\n\t\t\tclaimant_id TEXT NOT NULL,\n\t\t\trange_min   BLOB NOT NULL,\n\t\t\trange_max   BLOB NOT NULL,\n\t\t\tcodec       TEXT NOT NULL\n\t\t) STRICT", options)
	case sqlStatement313:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS fixed_actor_manifest_entries (\n\t\t\tactor_id  TEXT PRIMARY KEY REFERENCES agents(id),\n\t\t\tnamespace TEXT NOT NULL REFERENCES actor_namespace_claims(namespace),\n\t\t\tkind_id   INTEGER NOT NULL REFERENCES agent_kinds(id),\n\t\t\tname      TEXT NOT NULL,\n\t\t\tmetadata  TEXT NOT NULL CHECK (json_valid(metadata)),\n\t\t\tUNIQUE (namespace, name)\n\t\t) STRICT", options)
	case sqlStatement314:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS task_attributions (\n\t\t\ttask_id          TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tactor_id         TEXT NOT NULL REFERENCES agents(id),\n\t\t\tfirst_journal_id INTEGER NOT NULL REFERENCES journal(journal_id),\n\t\t\tPRIMARY KEY (task_id, actor_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement315:
		return sqlitex.ExecuteTransient(conn, "CREATE VIEW IF NOT EXISTS journal_attributed AS\n\t\t SELECT j.journal_id AS journal_id,j.kind_id AS kind_id,COALESCE(j.actor_id,anchor.actor_id) AS effective_actor_id,\n\t\t j.recorded_at AS recorded_at,j.produced_by_operation_journal_id AS produced_by_operation_journal_id\n\t\t FROM journal j LEFT JOIN journal anchor ON anchor.journal_id=j.produced_by_operation_journal_id", options)
	case sqlStatement316:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a\n\t\t\tLEFT JOIN journal_authority_bootstraps d ON d.journal_id = a.journal_id\n\t\t\tWHERE a.authority_kind_id = ?1 AND d.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement317:
		return sqlitex.Execute(conn, "SELECT a.journal_id FROM journal_authorities a\n\t\t\tLEFT JOIN journal_authority_assignment_transitions d ON d.journal_id = a.journal_id\n\t\t\tWHERE a.authority_kind_id = ?1 AND d.journal_id IS ?2 LIMIT ?3", appendStaticSQLArgs(options, nil, 1))
	case sqlStatement318:
		return sqlitex.Execute(conn, "SELECT d.journal_id FROM journal_authority_bootstraps d\n\t\t\tJOIN journal_authorities a ON a.journal_id = d.journal_id\n\t\t\tWHERE a.authority_kind_id <> ?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement319:
		return sqlitex.Execute(conn, "SELECT d.journal_id FROM journal_authority_assignment_transitions d\n\t\t\tJOIN journal_authorities a ON a.journal_id = d.journal_id\n\t\t\tWHERE a.authority_kind_id <> ?1 LIMIT ?2", appendStaticSQLArgs(options, 1))
	case sqlStatement320:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS authority_kinds (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement321:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS assignment_slots (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement322:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS assignment_transitions (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE) STRICT", options)
	case sqlStatement323:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_operations (\n\t\t\tjournal_id            INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\toperation_id         TEXT NOT NULL UNIQUE,\n\t\t\tauthority_journal_id INTEGER REFERENCES journal_authorities(journal_id),\n\t\t\tcommand_digest       BLOB NOT NULL,\n\t\t\tmutation_digest      BLOB NOT NULL,\n\t\t\tmutation_encoding_version TEXT,\n\t\t\tcanonical_mutation   BLOB,\n\t\t\tCHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR\n\t\t\t       (length(mutation_encoding_version) > 0 AND length(canonical_mutation) > 0))\n\t\t) STRICT", options)
	case sqlStatement324:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_insert\n\t\t BEFORE INSERT ON journal_operations\n\t\t WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR\n\t\t\t           (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0))\n\t\t BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END", options)
	case sqlStatement325:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER IF NOT EXISTS journal_operations_canonical_update\n\t\t BEFORE UPDATE OF mutation_encoding_version, canonical_mutation ON journal_operations\n\t\t WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR\n\t\t\t           (length(NEW.mutation_encoding_version) > 0 AND length(NEW.canonical_mutation) > 0))\n\t\t BEGIN SELECT RAISE(ABORT, 'invalid canonical mutation version/bytes pair'); END", options)
	case sqlStatement326:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_operation_result_slots (\n\t\t\tjournal_id           INTEGER NOT NULL REFERENCES journal_operations(journal_id),\n\t\t\tresult_slot_id      TEXT NOT NULL,\n\t\t\tproduced_journal_id INTEGER NOT NULL REFERENCES journal(journal_id),\n\t\t\tPRIMARY KEY (journal_id, result_slot_id)\n\t\t) STRICT, WITHOUT ROWID", options)
	case sqlStatement327:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authorities (\n\t\t\tjournal_id              INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tauthority_kind_id      INTEGER NOT NULL REFERENCES authority_kinds(id),\n\t\t\toperation_authority_id TEXT NOT NULL UNIQUE\n\t\t) STRICT", options)
	case sqlStatement328:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_bootstraps (\n\t\t\tjournal_id INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id),\n\t\t\tlabel     TEXT NOT NULL\n\t\t) STRICT", options)
	case sqlStatement329:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_assignment_episodes (\n\t\t\tassignment_id             TEXT PRIMARY KEY,\n\t\t\ttask_id                   TEXT NOT NULL REFERENCES tasks(id),\n\t\t\tslot_id                   INTEGER NOT NULL REFERENCES assignment_slots(id),\n\t\t\tactor_id                  TEXT NOT NULL REFERENCES agents(id),\n\t\t\tpredecessor_assignment_id TEXT UNIQUE REFERENCES journal_authority_assignment_episodes(assignment_id),\n\t\t\t-- ParentAssignmentID (§14.5): deliberate governance-citation edge, cited at\n\t\t\t-- start; NOT UNIQUE (one parent may govern many children), distinct from the\n\t\t\t-- UNIQUE predecessor (succession) edge above.\n\t\t\tparent_assignment_id      TEXT REFERENCES journal_authority_assignment_episodes(assignment_id)\n\t\t) STRICT", options)
	case sqlStatement330:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_authority_assignment_transitions (\n\t\t\tjournal_id     INTEGER PRIMARY KEY REFERENCES journal_authorities(journal_id),\n\t\t\tassignment_id TEXT NOT NULL REFERENCES journal_authority_assignment_episodes(assignment_id),\n\t\t\ttransition_id INTEGER NOT NULL REFERENCES assignment_transitions(id),\n\t\t\tUNIQUE (assignment_id, transition_id)\n\t\t) STRICT", options)
	case sqlStatement331:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_decisions (\n\t\t\tjournal_id     INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tdecision_kind TEXT NOT NULL,\n\t\t\ttask_id       TEXT REFERENCES tasks(id),\n\t\t\tpayload       TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case sqlStatement332:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE IF NOT EXISTS journal_evidence (\n\t\t\tjournal_id      INTEGER PRIMARY KEY REFERENCES journal(journal_id),\n\t\t\tevidence_kind  TEXT NOT NULL,\n\t\t\ttask_id        TEXT REFERENCES tasks(id),\n\t\t\tcontent_digest BLOB NOT NULL,\n\t\t\tpayload        TEXT NOT NULL CHECK (json_valid(payload))\n\t\t) STRICT", options)
	case sqlStatement333:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_transitions_assignment ON journal_authority_assignment_transitions (assignment_id)", options)
	case sqlStatement334:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_episodes_task ON journal_authority_assignment_episodes (task_id)", options)
	case sqlStatement335:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_episodes_parent ON journal_authority_assignment_episodes (parent_assignment_id)", options)
	case sqlStatement336:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END", options)
	case sqlStatement337:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version,canonical_mutation ON journal_operations WHEN NOT ((NEW.mutation_encoding_version IS NULL AND NEW.canonical_mutation IS NULL) OR (length(NEW.mutation_encoding_version)>0 AND length(NEW.canonical_mutation)>0)) BEGIN SELECT RAISE(ABORT,'invalid canonical mutation version/bytes pair'); END", options)
	case sqlStatement338:
		return sqlitex.ExecuteTransient(conn, "DROP VIEW IF EXISTS journal_attributed", options)
	case sqlStatement339:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_new (\n\t\t\tjournal_id   INTEGER PRIMARY KEY AUTOINCREMENT,\n\t\t\tkind_id     INTEGER NOT NULL REFERENCES journal_kinds(id),\n\t\t\tactor_id    TEXT REFERENCES agents(id),\n\t\t\trecorded_at INTEGER NOT NULL,\n\t\t\tproduced_by_operation_journal_id INTEGER REFERENCES journal_operations(journal_id),\n\t\t\tCHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),\n\t\t\tCHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)\n\t\t) STRICT", options)
	case sqlStatement340:
		return sqlitex.Execute(conn, "INSERT INTO journal_new (journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id)\n\t\t\tSELECT journal_id, kind_id, actor_id, recorded_at, produced_by_operation_journal_id FROM journal", options)
	case sqlStatement341:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_new RENAME TO journal", options)
	case sqlStatement342:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX IF NOT EXISTS idx_journal_pboj  ON journal (produced_by_operation_journal_id)", options)
	case sqlStatement343:
		return sqlitex.ExecuteTransient(conn, "CREATE VIEW IF NOT EXISTS journal_attributed AS SELECT j.journal_id AS journal_id,j.kind_id AS kind_id,COALESCE(j.actor_id,anchor.actor_id) AS effective_actor_id,j.recorded_at AS recorded_at,j.produced_by_operation_journal_id AS produced_by_operation_journal_id FROM journal j LEFT JOIN journal anchor ON anchor.journal_id=j.produced_by_operation_journal_id", options)
	case sqlStatement344:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_legacy (journal_id INTEGER PRIMARY KEY AUTOINCREMENT,kind_id INTEGER NOT NULL REFERENCES journal_kinds(id),actor_id TEXT REFERENCES agents(id),recorded_at INTEGER NOT NULL,produced_by_operation_journal_id INTEGER,CHECK ((actor_id IS NULL) = (produced_by_operation_journal_id IS NOT NULL)),CHECK (kind_id <> 1 OR produced_by_operation_journal_id IS NOT NULL)) STRICT", options)
	case sqlStatement345:
		return sqlitex.Execute(conn, "INSERT INTO journal_legacy SELECT * FROM journal", options)
	case sqlStatement346:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_legacy RENAME TO journal", options)
	case sqlStatement347:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_kind ON journal(kind_id)", options)
	case sqlStatement348:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_actor ON journal(actor_id)", options)
	case sqlStatement349:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_pboj ON journal(produced_by_operation_journal_id)", options)
	case sqlStatement350:
		return sqlitex.ExecuteTransient(conn, "CREATE INDEX idx_journal_recorded_at ON journal(recorded_at,journal_id)", options)
	case sqlStatement351:
		return sqlitex.ExecuteTransient(conn, "CREATE TABLE journal_operations_v1 (journal_id INTEGER PRIMARY KEY REFERENCES journal(journal_id),operation_id TEXT NOT NULL UNIQUE,authority_journal_id INTEGER REFERENCES journal_authorities(journal_id),command_digest BLOB NOT NULL,mutation_digest BLOB NOT NULL,mutation_encoding_version TEXT,canonical_mutation BLOB,CHECK ((mutation_encoding_version IS NULL AND canonical_mutation IS NULL) OR (mutation_encoding_version='provenance.mutation.v1' AND length(canonical_mutation)>0))) STRICT", options)
	case sqlStatement352:
		return sqlitex.Execute(conn, "INSERT INTO journal_operations_v1 SELECT * FROM journal_operations", options)
	case sqlStatement353:
		return sqlitex.ExecuteTransient(conn, "ALTER TABLE journal_operations_v1 RENAME TO journal_operations", options)
	case sqlStatement354:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_insert BEFORE INSERT ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END", options)
	case sqlStatement355:
		return sqlitex.ExecuteTransient(conn, "CREATE TRIGGER journal_operations_canonical_update BEFORE UPDATE OF mutation_encoding_version ON journal_operations WHEN NEW.mutation_encoding_version IS NOT NULL AND NEW.mutation_encoding_version!='provenance.mutation.v1' BEGIN SELECT RAISE(ABORT,'V1 only'); END", options)
	case sqlStatement356:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_tasks (\n\t\t\tid              TEXT PRIMARY KEY,\n\t\t\tnamespace       TEXT,\n\t\t\ttitle           TEXT,\n\t\t\tdescription     TEXT,\n\t\t\towner_id        TEXT,\n\t\t\tstatus_id       INTEGER,\n\t\t\tpriority_id     INTEGER,\n\t\t\ttype_id         INTEGER,\n\t\t\tphase_id        INTEGER,\n\t\t\tnotes           TEXT,\n\t\t\tcreated_at      INTEGER,\n\t\t\tupdated_at      INTEGER,\n\t\t\tclosed_at       INTEGER,\n\t\t\tclose_reason    TEXT,\n\t\t\tlast_journal_id INTEGER\n\t\t)", options)
	case sqlStatement357:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_task_attributions (\n\t\t\ttask_id          TEXT NOT NULL,\n\t\t\tactor_id         TEXT NOT NULL,\n\t\t\tfirst_journal_id INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (task_id, actor_id)\n\t\t)", options)
	case sqlStatement358:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_edges (\n\t\t\tsource_id  TEXT NOT NULL,\n\t\t\ttarget_id  TEXT NOT NULL,\n\t\t\tkind_id    INTEGER NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL,\n\t\t\tPRIMARY KEY (source_id, target_id, kind_id)\n\t\t)", options)
	case sqlStatement359:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_labels (\n\t\t\ttask_id TEXT NOT NULL,\n\t\t\tname    TEXT NOT NULL,\n\t\t\tPRIMARY KEY (task_id, name)\n\t\t)", options)
	case sqlStatement360:
		return sqlitex.ExecuteTransient(conn, "CREATE TEMP TABLE shadow_comments (\n\t\t\tid         TEXT PRIMARY KEY,\n\t\t\ttask_id    TEXT NOT NULL,\n\t\t\tauthor_id  TEXT NOT NULL,\n\t\t\tbody       TEXT NOT NULL,\n\t\t\tcreated_at INTEGER NOT NULL\n\t\t)", options)
	case sqlStatement361:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_tasks", options)
	case sqlStatement362:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_task_attributions", options)
	case sqlStatement363:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_edges", options)
	case sqlStatement364:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_labels", options)
	case sqlStatement365:
		return sqlitex.ExecuteTransient(conn, "DROP TABLE IF EXISTS shadow_comments", options)
	default:
		return fmt.Errorf("%w: id %d; update the exhaustive SQL dispatcher", errUnknownSQLStatement, statement)
	}
}

func appendStaticSQLArgs(options *sqlitex.ExecOptions, values ...any) *sqlitex.ExecOptions {
	result := &sqlitex.ExecOptions{}
	if options != nil {
		*result = *options
		result.Args = append([]any(nil), options.Args...)
	}
	result.Args = append(result.Args, values...)
	return result
}
