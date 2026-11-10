package meta

// exiftoolOptionNames is the set of names ExifTool treats as
// command-line options (lowercase). A bare tag selector whose name
// is in this set must never reach the argv: prefixed with "-" it
// would execute the option instead of selecting a tag, and some
// options write files (-w/-textOut, -W/-tagOut, -o, -overwrite_original,
// -savePath/-saveFormat and friends), which would break the
// read-only guarantee of this server.
//
// The set is the union of every authoritative source in the
// ExifTool 13.55 distribution, because an incomplete list is
// exactly how earlier aliases (textOut, tagOut, printConv,
// extractEmbedded, groupNames, binary, json) slipped through:
//
//   - the @availableOptions lookup table in Image/ExifTool.pm
//     (the library's own registry: "All options must exist in
//     this lookup");
//   - the application option chain and the documented options in
//     the exiftool POD (application-level options and aliases);
//   - every single letter (most letters are option letters);
//   - format and argfile keywords accepted in option position.
//
// ValidateSelector also checks the name with trailing digits
// trimmed, which covers parameterized families (-fast2, -ee3,
// -g4, -htmldump0, ...) whose base name is listed here.
// Regenerate by re-extracting from those sources when ExifTool
// is upgraded; TestValidateSelectorRejectsWriteOptions pins the
// dangerous names explicitly.
var exiftoolOptionNames = map[string]bool{
	"a": true, "addtagsfromfile": true, "all": true, "api": true,
	"argformat": true, "args": true, "array": true, "b": true,
	"binary": true, "byteorder": true, "byteunit": true, "bz2": true,
	"c": true, "charset": true, "charsetexif": true, "charsetfilename": true,
	"charsetid3": true, "charsetiptc": true, "charsetphotoshop": true, "charsetquicktime": true,
	"charsetriff": true, "common_args": true, "compact": true, "composite": true,
	"compress": true, "config": true, "coordformat": true, "csv": true,
	"csvdelim": true, "d": true, "dateformat": true, "debug": true,
	"decimal": true, "delete_original": true, "diff": true, "dir": true,
	"directory": true, "duplicates": true, "e": true, "ec": true,
	"echo": true, "echo1": true, "echo2": true, "echo3": true,
	"echo4": true, "ee": true, "efile": true, "efile2": true,
	"efile3": true, "else": true, "elsif": true, "encodehangs": true,
	"endif": true, "escape": true, "escapec": true, "escapehtml": true,
	"escapexml": true, "ex": true, "exclude": true, "execute": true,
	"exiftool": true, "ext": true, "extendedxmp": true, "extension": true,
	"extractembedded": true, "f": true, "false": true, "fast": true,
	"fast1": true, "fast2": true, "fast3": true, "fast4": true,
	"fast5": true, "fastscan": true, "file": true, "file1": true,
	"file2": true, "filename": true, "filenum": true, "fileorder": true,
	"fileorder4": true, "filter": true, "filterw": true, "fixbase": true,
	"forceprint": true, "format": true, "g": true, "g0": true,
	"g1": true, "g3": true, "g4": true, "geohposerr": true,
	"geolocaltnames": true, "geolocation": true, "geolocfeature": true, "geolocmaxdist": true,
	"geolocminpop": true, "geomaxextsecs": true, "geomaxhdop": true, "geomaxintsecs": true,
	"geomaxpdop": true, "geominsats": true, "geospeedref": true, "geotag": true,
	"geousertag": true, "globaltimeshift": true, "gpsquadrant": true, "groupheadings": true,
	"groupnames": true, "gz": true, "h": true, "h2": true,
	"hash": true, "hex": true, "hextagids": true, "htmldump": true,
	"htmldump0": true, "htmldumpbase": true, "htmlformat": true, "i": true,
	"if": true, "if5": true, "ignore": true, "ignoregroups": true,
	"ignoreminorerrors": true, "ignoretags": true, "imagehashtype": true, "j": true,
	"json": true, "k": true, "keeputctime": true, "l": true,
	"lang": true, "largefilesupport": true, "latin": true, "limitlongvalues": true,
	"list": true, "list_dir": true, "listd": true, "listf": true,
	"listg": true, "listgeo": true, "listitem": true, "listjoin": true,
	"listr": true, "listsep": true, "listsplit": true, "listw": true,
	"listwf": true, "listx": true, "long": true, "m": true,
	"makernotes": true, "mditemtags": true, "missingtagvalue": true, "n": true,
	"nomandatory": true, "nomultiexif": true, "nop": true, "nopdflist": true,
	"nowarning": true, "o": true, "out": true, "overwrite_original": true,
	"overwrite_original_in_place": true, "p": true, "password": true, "pause": true,
	"php": true, "plot": true, "preserve": true, "printconv": true,
	"printcsv": true, "printformat": true, "progress": true, "q": true,
	"quicktimehandler": true, "quicktimepad": true, "quicktimeutc": true, "quiet": true,
	"r": true, "recurse": true, "requestall": true, "requesttags": true,
	"require": true, "restore_original": true, "s": true, "s2": true,
	"savebin": true, "saveformat": true, "savepath": true, "scalar": true,
	"scanforxmp": true, "sep": true, "separator": true, "short": true,
	"sort": true, "sort2": true, "sourcefile": true, "srcfile": true,
	"stay_open": true, "strictdate": true, "strip": true, "struct": true,
	"structformat": true, "systemtags": true, "systemtimeres": true, "t": true,
	"tab": true, "table": true, "tagout": true, "tagoutext": true,
	"tagsfromfile": true, "textout": true, "timezone": true, "true": true,
	"u": true, "undeftags": true, "unknown": true, "unknown2": true,
	"use": true, "userparam": true, "v": true, "v0": true,
	"v2": true, "validate": true, "ver": true, "verbose": true,
	"veryshort": true, "w": true, "wext": true, "window": true,
	"windowslongpath": true, "windowswidefile": true, "wm": true, "writemode": true,
	"wtext": true, "x": true, "xattrtags": true, "xml": true,
	"xmlformat": true, "xmpautoconv": true, "xmpshorthand": true, "y": true,
	"z": true, "zip": true,
}
