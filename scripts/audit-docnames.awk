# A doc comment must name the declaration it sits on.
#
# Inserting a function between an existing doc comment and the function it
# documents silently reattaches that comment to the new declaration. Go's
# tooling reports nothing: both compile, both vet clean, and `go doc` cheerfully
# prints the wrong prose under the wrong signature. That has happened four times
# in this repository, which is why it is checked rather than remembered.
#
# The rule is deliberately narrow. A comment is flagged only when its first word
# is itself a declaration in the same file, which is the shape the mistake
# takes; prose openers ("Falls back to...") are left alone.
#
# Usage: awk -f audit-docnames.awk file.go file.go   (same file twice: pass 1
# collects the declared names, pass 2 checks the comments against them)

function declname(line,   m) {
	if (line ~ /^func[ \t]/) {
		m = line
		sub(/^func[ \t]+/, "", m)
		sub(/^\([^)]*\)[ \t]*/, "", m)   # method receiver
		sub(/[ \t]*[\(\[].*$/, "", m)
		return m
	}
	if (line ~ /^type[ \t]/) {
		m = line
		sub(/^type[ \t]+/, "", m)
		sub(/[ \t].*$/, "", m)
		return m
	}
	return ""
}

FNR == NR {                                  # pass 1: what this file declares
	n = declname($0)
	if (n != "") declared[n] = 1
	next
}

{                                            # pass 2: check each doc comment
	if ($0 ~ /^\/\//) {
		if (!inblock) { inblock = 1; first = $0; firstline = FNR }
		next
	}
	n = declname($0)
	if (inblock && n != "") {
		word = first
		sub(/^\/\/[ \t]*/, "", word)
		sub(/[^A-Za-z0-9_].*$/, "", word)
		if (word != "" && word != n && (word in declared)) {
			printf "%s:%d: doc comment says %s, declaration is %s\n", FILENAME, firstline, word, n
			bad = 1
		}
	}
	inblock = 0
}

END { exit bad }
