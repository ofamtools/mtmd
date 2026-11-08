// Package coldlog is an append-only, compressed, on-disk log of scrollback
// rows: the cold half of a pane's history. The emulator's evict hook hands
// rows to Log.Append; Reader gives random access by row number.
//
// Layout of one log (a directory):
//
//	LOCK                  advisory lock plus the owner's pid
//	seg-<first-row>.cold  write-once segments, named by their first row id
//	closed.json           orphan marker, present while no pane owns the log
//
// A segment is a 24-byte header (magic, file version, vt row format version,
// first row id), then blocks of up to 256 rows, each
//
//	[u32 compressed length][u32 crc32 of payload][u64 first row id]
//	[u32 row count][u32 raw length][gzip(rows)]
//
// where a row is [uvarint width][flags byte][uvarint len][vt-encoded bytes].
// A sealed segment ends with a footer: a table of the block headers with their
// offsets, then a 24-byte trailer (table offset, block count, table crc,
// magic). A segment without a valid footer is the one a crash tore: its blocks
// are walked, the first bad one ends it, and opening the log truncates the rest
// and seals it.
//
// Row ids are global and monotonic. Rows dropped because the queue was full
// still take their ids, so a gap is a jump between the end of one block and
// the first id of the next, and the ids of the rows after it stay aligned with
// the emulator's count. A gap at the very end, with no row after it yet, is
// carried by the writer and shows on disk once a row follows.
//
// Nothing here knows about panes, sessions or the daemon.
package coldlog
