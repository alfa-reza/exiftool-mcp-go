package meta

// exiftoolOptionNames is the set of names the ExifTool application
// treats as command-line options when they appear as "-name" in argv.
//
// A tag selector becomes an argv element "-" + selector, so an
// unqualified selector whose name collides with an option would be
// executed as that option instead of selecting a tag (for example
// "-config", "-stay_open" or "-restore_original"). ValidateSelector
// rejects such selectors; the qualified "Group:Name" form is never an
// option and stays available for tags that share a name with an option.
//
// The list was extracted from the ExifTool 13.55 application itself
// (its argument parser tables and POD option index), plus a small
// supplement of documented options the parser handles in other forms.
// Regenerate it when the supported ExifTool version changes.
var exiftoolOptionNames = map[string]bool{
	"a": true, "all": true, "api": true, "args": true,
	"b": true, "bz2": true,
	"c": true, "charset": true, "common_args": true, "composite": true,
	"config": true, "csv": true, "csvdelim": true,
	"d": true, "dateformat": true, "delete_original": true, "diff": true,
	"dir": true, "directory": true, "duplicates": true,
	"e": true, "echo": true, "echo2": true, "ee": true, "efile": true,
	"else": true, "elsif": true, "endif": true, "execute": true,
	"exiftool": true, "ext": true,
	"f": true, "false": true, "fast": true, "file": true, "filename": true,
	"fileorder": true, "filter": true, "format": true,
	"g": true, "geotag": true, "globaltimeshift": true, "gz": true,
	"h": true, "h2": true, "htmldump": true,
	"i": true, "if": true, "ignoreminorerrors": true,
	"j": true,
	"k": true,
	"l": true, "lang": true, "list": true, "list_dir": true,
	"listd": true, "listf": true, "listg": true, "listitem": true,
	"listr": true, "listw": true, "listx": true,
	"m": true,
	"n": true,
	"o": true, "overwrite_original": true,
	"overwrite_original_in_place": true,
	"p":                           true, "password": true, "pause": true, "php": true, "plot": true,
	"progress": true,
	"q":        true,
	"r":        true, "requestall": true, "restore_original": true,
	"s": true, "scanforxmp": true, "sep": true, "sort": true,
	"sourcefile": true, "srcfile": true, "stay_open": true,
	"strip": true, "struct": true,
	"t": true, "tagoutext": true, "tagsfromfile": true, "true": true,
	"u": true, "unknown": true, "use": true, "userparam": true,
	"v": true, "validate": true, "ver": true,
	"w": true, "wext": true, "window": true, "wm": true, "wtext": true,
	"x": true, "xml": true,
	"z": true, "zip": true,
}
