package main

import "fmt"

// translator puts the chat's own texts in another language: each is looked
// up by its English format, and stays in English without a translation.
// The nil translator is English.
type translator map[string]string

// f formats format, translated, with args.
func (t translator) f(format string, args ...any) string {
	if s, ok := t[format]; ok {
		format = s
	}
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// spanish is the chat's texts in Spanish.
var spanish = translator{
	// Notes of the chat.
	"not answered":                                         "sin responder",
	"could not send %q: %v":                                "no pude enviar %q: %v",
	"handed DOGGIE over":                                   "DOGGIE pasó a otro proceso",
	"the DOGGIE run here stopped: %v":                      "el DOGGIE de esta ventana se detuvo: %v",
	"not sent: the text is not valid UTF-8":                "no enviado: el texto no es UTF-8 válido",
	"not sent: %d characters, the limit is %d":             "no enviado: %d caracteres, el límite es %d",
	"DOGGIE is moving to another process...":               "DOGGIE se está pasando a otro proceso…",
	"the chat that ran DOGGIE closed":                      "se cerró el chat donde corría DOGGIE",
	"could not start DOGGIE here: %v":                      "no pude arrancar DOGGIE aquí: %v",
	"doggie run is taking DOGGIE over; handing it over...": "doggie run se queda con DOGGIE; se lo paso…",
	"the turn failed: %s":                                  "el turno falló: %s",
	"lost the connection to DOGGIE; reconnecting...":       "se perdió la conexión con DOGGIE; reconectando…",
	"connected again":                                      "conectado otra vez",
	"DOGGIE ended the connection: %s":                      "DOGGIE cerró la conexión: %s",
	"could not read what was said meanwhile: %v":           "no pude leer lo que se dijo mientras tanto: %v",
	"no reply yet: is DOGGIE's brain running?":             "todavía sin respuesta: ¿está corriendo el cerebro de DOGGIE?",
	"stopping DOGGIE...":                                   "deteniendo DOGGIE…",
	"no config file at %s, using defaults":                 "no hay config en %s; uso los valores por defecto",
	"could not read the conversation so far: %v":           "no pude leer la conversación hasta ahora: %v",
	"earlier in this conversation, from %s ago:":           "antes en esta conversación, desde hace %s:",
	"DOGGIE was stopped; it's back when someone starts it (doggie run or doggie chat)": "DOGGIE se detuvo; vuelve cuando alguien lo arranque (doggie run o doggie chat)",
	"talking with DOGGIE at %s on %s; /help lists the commands, Ctrl+D quits":          "hablando con DOGGIE en %s por %s; /help muestra los comandos, Ctrl+D sale",
	"DOGGIE wasn't running: started it here, logging to %s; it stops when you quit":    "DOGGIE no estaba corriendo: lo arranqué aquí, con el log en %s; se detiene al salir",
	"DOGGIE was gone: started it here, logging to %s; it stops when you quit":          "DOGGIE ya no estaba: lo arranqué aquí, con el log en %s; se detiene al salir",
	"DOGGIE is running, logging to %s; /help lists the commands, Ctrl+D stops it":      "DOGGIE está corriendo, con el log en %s; /help muestra los comandos, Ctrl+D lo detiene",
	"DOGGIE stopped": "DOGGIE se detuvo",
	"DOGGIE keeps running; doggie opens the chat again": "DOGGIE sigue corriendo; doggie vuelve a abrir el chat",

	// Why a turn failed.
	"it took too long":             "tardó demasiado",
	"DOGGIE's tools didn't answer": "las herramientas de DOGGIE no respondieron",
	`its LLM isn't logged in; log in with claude, or run "doggie login" for the backup`: `su LLM no tiene sesión; entra con claude, o corre "doggie login" para el respaldo`,

	// Tasks.
	"%s asks: %q":                   "%s pregunta: %q",
	"%s finished":                   "%s terminó",
	"%s failed: %s":                 "%s falló: %s",
	"%s was cancelled":              "%s se canceló",
	"%s asked a sensitive question": "%s hizo una pregunta sensible",
	"%s asked: %s":                  "%s preguntó: %s",
	"%s changed":                    "%s cambió",
	"%s: asks (/answer %s …)":       "%s: pregunta (/answer %s …)",
	". Only you may answer it: /answer %s <answer>": ". Solo tú puedes responderla: /answer %s <respuesta>",
	"+%d elsewhere":             "+%d en otras",
	"%s in other conversations": "%s en otras conversaciones",
	"1 task":                    "1 tarea",
	"%d tasks":                  "%d tareas",
	"no conversation":           "sin conversación",
	"conversation %s":           "conversación %s",
	"pending":                   "pendiente",
	"running":                   "en curso",
	"waiting_input":             "espera respuesta",
	"done":                      "terminada",
	"failed":                    "falló",
	"cancelled":                 "cancelada",

	// Commands.
	"usage: /tasks [all]":                             "uso: /tasks [all]",
	"usage: /answer <id> <text>, e.g. /answer t3 yes": "uso: /answer <id> <texto>, p. ej. /answer t3 sí",
	"usage: /cancel <id> [why]":                       "uso: /cancel <id> [motivo]",
	`unknown command %s; /help lists them, and //text says text starting with "/"`: `comando desconocido %s; /help los muestra, y //texto dice un texto que empieza con "/"`,
	"could not list the tasks: %s": "no pude listar las tareas: %s",
	"could not cancel %s: %s":      "no pude cancelar %s: %s",
	"cancelled %s":                 "cancelé %s",
	"could not answer %s: %s":      "no pude responder %s: %s",
	"could not answer %s: it isn't waiting for an answer (it is %s)": "no pude responder %s: no espera respuesta (está %s)",
	"answered %s, though not with one of its options (%s)":           "respondí %s, aunque no con una de sus opciones (%s)",
	"answered %s":                         "respondí %s",
	"DOGGIE's task manager isn't running": "el gestor de tareas de DOGGIE no está corriendo",
	"DOGGIE didn't answer in time":        "DOGGIE no respondió a tiempo",
	"no tasks in progress":                "no hay tareas en curso",
	"no tasks":                            "no hay tareas",
	"%s in progress:":                     "%s en curso:",
	"the last %s:":                        "las últimas %s:",
	"ID\tSTATUS\tAGE\tTASK\tDETAIL":       "ID\tESTADO\tEDAD\tTAREA\tDETALLE",
	"(and %s before)":                     "(y %s antes)",
	"answer for %s (hidden): ":            "respuesta para %s (oculta): ",
	chatHelp: `· comandos, los corre el chat, no el cerebro de DOGGIE:
  /tasks               tareas en curso; /tasks all suma las últimas que terminaron
  /answer <id> [texto] responde tú la pregunta de una tarea, la única vía para
                       las que DOGGIE no puede responder por ti; sin texto, la
                       escribes oculta
  /cancel <id> [motivo] cancela una tarea
  /help                esta lista
  //texto              dice un texto que empieza con "/"`,
}
