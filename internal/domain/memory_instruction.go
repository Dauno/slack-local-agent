package domain

import (
	"strings"
	"unicode"
)

func memoryReferenceWords(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

func containsSensitivePersonalData(value string) bool {
	if personalEmailPattern.MatchString(value) || personalPhonePattern.MatchString(value) || paymentCardPattern.MatchString(value) {
		return true
	}
	lower := strings.ToLower(value)
	for _, term := range []string{
		"social security number", "ssn", "national id", "government id", "passport number", "date of birth", "medical diagnosis", "medical record", "bank account",
		"número de seguridad social", "numero de seguridad social", "dni", "número de pasaporte", "numero de pasaporte", "fecha de nacimiento", "diagnóstico médico", "diagnostico medico", "historial médico", "historial medico", "cuenta bancaria",
	} {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func isInstructionLikeMemoryText(value string) bool {
	if isSpanishInstructionLikeMemoryText(value) {
		return true
	}
	for _, sentence := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '\n' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':'
	}) {
		words := memoryReferenceWords(sentence)
		if len(words) == 0 {
			continue
		}
		if words[0] == "please" || words[0] == "kindly" {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		if isMemoryCategoryDirective(words) {
			return true
		}
		if isPersistentAssistantInstruction(words) {
			return true
		}
		if isSafeMemoryReferenceIdentifier(sentence) {
			continue
		}
		if imperativeMemoryVerb(words[0]) || isFormatOrOutputDirective(words) || (len(words) > 1 && words[0] == "you" && modalMemoryVerb(words[1])) ||
			(len(words) > 2 && words[0] == "you" && words[1] == "are" && words[2] == "now") ||
			(len(words) > 1 && words[0] == "do" && words[1] == "not") || words[0] == "never" {
			return true
		}
	}
	return false
}

func isSafeMemoryReferenceIdentifier(value string) bool {
	value = strings.Trim(strings.TrimSpace(value), "-`*_#[]() \t")
	return value == string(CapReadFile)
}

func isSpanishInstructionLikeMemoryText(value string) bool {
	for _, sentence := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '\n' || r == '.' || r == '!' || r == '?' || r == ';' || r == ':'
	}) {
		words := memoryReferenceWords(sentence)
		if len(words) == 0 {
			continue
		}
		if len(words) > 1 && words[0] == "por" && words[1] == "favor" {
			words = words[2:]
		}
		if len(words) == 0 {
			continue
		}
		if spanishImperativeMemoryVerb(words[0]) || isSpanishMemoryCategoryDirective(words) {
			return true
		}
		if len(words) >= 3 && words[0] == "a" && words[1] == "partir" && words[2] == "de" {
			return true
		}
		if len(words) >= 3 && words[0] == "recuerda" && words[1] == "que" && spanishDirectiveModal(words[2]) {
			return true
		}
		if isSpanishPersistentAssistantInstruction(words) {
			return true
		}
	}
	return false
}

func isSpanishPersistentAssistantInstruction(words []string) bool {
	if len(words) >= 2 && (words[0] == "recuerda" || words[0] == "guarda") && words[1] == "que" {
		if spanishDirectiveClause(words[2:]) {
			return true
		}
	}
	if spanishDirectiveClause(words) {
		return true
	}
	return isSpanishPersistentAssistantInstructionLegacy(words)
}

func spanishDirectiveClause(words []string) bool {
	if len(words) == 0 {
		return false
	}
	index := spanishDirectivePrefix(words)
	if index < len(words) && spanishDirectiveModal(words[index]) {
		index++
		if index < len(words) && words[index] == "que" {
			index++
		}
		index += spanishDirectivePrefix(words[index:])
		return index < len(words) && spanishDirectiveAction(words[index])
	}
	if index+1 < len(words) && (words[index] == "el" || words[index] == "la") && words[index+1] == "asistente" {
		index += 2
		index += spanishDirectivePrefix(words[index:])
		if index >= len(words) || !spanishDirectiveModal(words[index]) {
			return false
		}
		index++
		if index < len(words) && words[index] == "que" {
			index++
		}
		index += spanishDirectivePrefix(words[index:])
		return index < len(words) && spanishDirectiveAction(words[index])
	}
	return false
}

func spanishDirectivePrefix(words []string) int {
	index := 0
	for index < len(words) {
		switch words[index] {
		case "no", "siempre", "nunca", "jamás", "jamas":
			index++
		default:
			return index
		}
	}
	return index
}

func isSpanishPersistentAssistantInstructionLegacy(words []string) bool {
	if len(words) >= 2 && spanishDirectiveModal(words[0]) && spanishDirectiveAction(words[1]) {
		return true
	}
	if len(words) >= 3 && (words[0] == "el" || words[0] == "la") && words[1] == "asistente" && spanishDirectiveModal(words[2]) {
		return true
	}
	if len(words) >= 3 && words[0] == "en" && (words[1] == "cada" || words[1] == "todos" || words[1] == "todas") &&
		(words[2] == "mensaje" || words[2] == "mensajes" || words[2] == "respuesta" || words[2] == "respuestas") {
		return spanishDirectiveSequence(words[3:])
	}
	return len(words) >= 3 && words[0] == "a" && words[1] == "partir" && words[2] == "de"
}

func spanishDirectiveSequence(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if spanishDirectiveAction(words[0]) || spanishImperativeMemoryVerb(words[0]) {
		return true
	}
	return len(words) >= 2 && spanishDirectiveModal(words[0]) && spanishDirectiveAction(words[1])
}

func spanishDirectiveAction(word string) bool {
	switch word {
	case "responder", "contestar", "usar", "incluir", "mencionar", "revelar", "divulgar", "ejecutar", "ignorar", "omitir", "cambiar", "modificar", "eliminar", "borrar":
		return true
	}
	return false
}

func isSpanishMemoryCategoryDirective(words []string) bool {
	if len(words) == 0 {
		return false
	}
	start := 0
	if words[start] == "la" || words[start] == "el" || words[start] == "las" || words[start] == "los" {
		start++
	}
	if start == len(words) {
		return false
	}
	switch words[start] {
	case "instrucción", "instrucciones", "prompt", "política", "politica", "herramienta", "herramientas", "autorización", "autorizacion", "permiso", "permisos":
		return len(words) > start+1 && spanishImperativeMemoryVerb(words[start+1])
	}
	return false
}

func spanishImperativeMemoryVerb(word string) bool {
	switch word {
	case "ignora", "omite", "anula", "elude", "ejecuta", "corre", "usa", "llama", "revela", "divulga", "extrae", "concede", "permite", "deniega", "habilita", "deshabilita", "cambia", "modifica", "elimina", "borra", "escribe", "lee", "responde", "contesta":
		return true
	}
	return false
}

func spanishDirectiveModal(word string) bool {
	switch word {
	case "debe", "debes", "deben", "deberá", "debera", "deberán", "deberan", "deberías", "deberias", "tiene", "tienes", "tienen", "puede", "puedes", "pueden":
		return true
	}
	return false
}

func isPersistentAssistantInstruction(words []string) bool {
	if len(words) >= 3 && words[0] == "always" && words[1] == "answer" && words[2] == "every" {
		return true
	}
	if len(words) >= 3 && words[0] == "from" && words[1] == "now" && words[2] == "on" {
		return true
	}
	if len(words) >= 4 && words[0] == "for" && words[1] == "every" && words[2] == "future" && (words[3] == "response" || words[3] == "responses" || words[3] == "reply" || words[3] == "replies") {
		return true
	}
	if len(words) >= 3 && words[0] == "make" && words[1] == "sure" && words[2] == "to" {
		return true
	}
	if len(words) >= 2 && words[0] == "remember" && words[1] == "to" {
		return true
	}
	if len(words) >= 3 && words[0] == "answer" && words[1] == "every" && (words[2] == "request" || words[2] == "requests" || words[2] == "future") {
		return true
	}
	if len(words) >= 3 && words[0] == "the" && words[1] == "assistant" && modalMemoryVerb(words[2]) {
		return true
	}
	if len(words) >= 2 && words[0] == "assistant" && modalMemoryVerb(words[1]) {
		return true
	}
	if len(words) >= 4 && words[0] == "in" && (words[1] == "every" || words[1] == "each" || words[1] == "all") && (words[2] == "response" || words[2] == "responses" || words[2] == "reply" || words[2] == "replies") {
		return isScopedMemoryDirective(words[3:])
	}
	return false
}

func isMemoryCategoryDirective(words []string) bool {
	if len(words) == 0 {
		return false
	}
	start := 0
	if words[start] == "the" {
		start++
	}
	if start == len(words) {
		return false
	}

	consumed := 0
	switch words[start] {
	case "instruction", "instructions", "prompt", "policy", "tool", "tools", "function", "command", "authorization", "permission", "privilege":
		consumed = 1
	case "system", "developer", "model":
		if start+1 < len(words) && (words[start+1] == "prompt" || words[start+1] == "instruction" || words[start+1] == "instructions" || words[start+1] == "message" || words[start+1] == "policy" || words[start+1] == "tool" || words[start+1] == "tools") {
			consumed = 2
		}
	}
	if consumed == 0 {
		return false
	}

	tail := words[start+consumed:]
	if len(tail) > 0 && (tail[0] == "request" || tail[0] == "claim" || tail[0] == "directive") {
		tail = tail[1:]
	}
	return isDirectiveWords(tail)
}

func isDirectiveWords(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if words[0] == "please" || words[0] == "kindly" {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	if imperativeMemoryVerb(words[0]) {
		return true
	}
	switch words[0] {
	case "answer", "include", "mention", "state", "provide", "begin", "end", "be":
		return true
	}
	return isFormatOrOutputDirective(words) ||
		(len(words) > 1 && words[0] == "you" && modalMemoryVerb(words[1])) ||
		(len(words) > 2 && words[0] == "the" && words[1] == "assistant" && modalMemoryVerb(words[2])) ||
		(len(words) > 1 && words[0] == "do" && words[1] == "not") || words[0] == "never"
}

func isFormatOrOutputDirective(words []string) bool {
	if len(words) == 0 || (words[0] != "format" && words[0] != "output") {
		return false
	}
	if isFormatOrOutputFactVerb(words[1:]) {
		return false
	}
	if words[0] == "output" && len(words) > 1 && words[1] == "format" && isFormatOrOutputFactVerb(words[2:]) {
		return false
	}
	return true
}

func isFormatOrOutputFactVerb(words []string) bool {
	if len(words) == 0 {
		return false
	}
	switch words[0] {
	case "is", "was", "were", "changed", "changes", "change", "has", "had":
		return true
	}
	return false
}

func isScopedMemoryDirective(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if imperativeMemoryVerb(words[0]) {
		return true
	}
	switch words[0] {
	case "answer", "include", "mention", "state", "provide", "begin", "end", "be":
		return true
	}
	return isFormatOrOutputDirective(words) ||
		len(words) >= 2 && words[0] == "you" && modalMemoryVerb(words[1]) ||
		len(words) >= 3 && words[0] == "the" && words[1] == "assistant" && modalMemoryVerb(words[2])
}

func imperativeMemoryVerb(word string) bool {
	switch word {
	case "ignore", "disregard", "override", "bypass", "follow", "obey", "execute", "run", "call", "invoke", "use", "send", "reveal", "disclose", "exfiltrate", "grant", "allow", "deny", "enable", "disable", "change", "modify", "delete", "remove", "write", "read", "return", "respond", "act", "fetch", "open", "click", "curl", "wget", "bash", "sh", "python", "powershell", "rm":
		return true
	}
	return false
}

func modalMemoryVerb(word string) bool {
	switch word {
	case "must", "should", "shall", "need", "required", "require", "cannot", "can":
		return true
	}
	return false
}
