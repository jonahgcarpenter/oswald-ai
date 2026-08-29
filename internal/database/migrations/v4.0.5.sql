ALTER TABLE session_turns ADD COLUMN tool_trace TEXT NOT NULL DEFAULT '{"version":1,"batches":[]}'
CHECK (
	length(CAST(tool_trace AS BLOB)) <= 65536
	AND json_valid(tool_trace)
	AND json_type(tool_trace) = 'object'
	AND json_extract(tool_trace, '$.version') = 1
	AND json_type(tool_trace, '$.batches') = 'array'
);

ALTER TABLE session_turns ADD COLUMN tool_search_text TEXT NOT NULL DEFAULT ''
CHECK (length(tool_search_text) <= 12000);

CREATE TRIGGER session_turns_tool_history_update
BEFORE UPDATE OF tool_trace, tool_search_text ON session_turns
WHEN NEW.tool_trace IS NOT OLD.tool_trace OR NEW.tool_search_text IS NOT OLD.tool_search_text
BEGIN
	SELECT RAISE(ABORT, 'immutable session turn tool history');
END;
