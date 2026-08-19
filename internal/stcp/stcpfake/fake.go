// Package stcpfake imita o cliente OFTP do banco segundo o que o manual v5.3 documenta.
//
// Ele existe porque a alternativa é inaceitável: não há ambiente de homologação para pagamento, a
// única conexão existente é a de PRODUÇÃO no convênio real, e um arquivo transmitido "para testar"
// vira dinheiro saindo da conta do cliente. Sem este duplo, os critérios de idempotência e de morte
// no meio só poderiam ser exercitados pagando alguém.
//
// O que ele imita, e de onde vem cada comportamento:
//
//   - §5, p.13 — o que é enviado com sucesso é REMOVIDO da pasta de SAÍDA e movido para BACKUP.
//     É essa movimentação que o agente lê como evidência física.
//   - §12, p.30 — cada transferência deixa uma linha no log posicional de 10 campos, com o código
//     da operação, o resultado e o nome do arquivo.
//   - §6, p.14 — o acionamento recebe modo e filtro; o duplo respeita o filtro, porque um duplo que
//     ignora o filtro esconderia exatamente o defeito que o CA7 quer barrar.
//
// O que ele NÃO imita, de propósito: rede, protocolo Odette, TLS, retentativa. Nada disso muda a
// decisão do agente, que se apoia na evidência física.
package stcpfake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// Behavior é o desfecho que o duplo encena para um arquivo.
type Behavior int

const (
	// Succeed — transmissão bem-sucedida: sai da SAÍDA, aparece em BACKUP, log com resultado de
	// sucesso.
	Succeed Behavior = iota
	// Reject — o cliente recusa: o arquivo PERMANECE na pasta de saída e o log registra o código de
	// erro. É o desfecho do CA2.
	Reject
	// Vanish — o arquivo some da SAÍDA sem aparecer em BACKUP. Não é capricho: é o que acontece se
	// o backup estiver desligado no perfil, combinação que o próprio manual deixa ambígua (§5 diz
	// que move para backup; a captura do §8 mostra o checkbox desmarcado). O agente precisa tratar
	// isto como AMBÍGUO, nunca como sucesso.
	Vanish
	// Crash — o processo não roda. Devolve erro de execução, sem tocar em nada.
	Crash
)

// Call registra um acionamento, para que o teste possa afirmar o que foi (ou não foi) chamado.
type Call struct {
	Mode   stcp.Mode
	Filter string
}

// Incoming é um arquivo que o banco entrega no próximo acionamento em modo de recepção.
type Incoming struct {
	Name    string
	Content []byte
	// Logged decide se a entrega deixa linha no log posicional.
	//
	// `false` encena o caso que o agente PRECISA tratar e que não é hipotético: a caixa é do
	// convênio, e um arquivo pode estar na pasta sem que o log daquele ciclo o explique. Um duplo
	// que sempre logasse tornaria o fallback inverificável.
	Logged bool
}

// Fake é o duplo do cliente.
type Fake struct {
	OutboundDir string
	BackupDir   string
	InboundDir  string
	LogPath     string
	Profile     string

	// Incoming é o que o banco entrega no próximo acionamento em modo R.
	Incoming []Incoming

	// Behavior decide o desfecho por arquivo. Nil significa sucesso para todos.
	Behavior func(fileName string) Behavior
	// FailureCode é o resultado gravado no log quando o desfecho é Reject. Vale contra a tabela do
	// §11 (pp. 24-29); o default é o erro de nome inválido/filtro (401).
	FailureCode string
	// Now controla o carimbo das linhas de log.
	Now func() time.Time

	mu    sync.Mutex
	calls []Call
}

// New monta o duplo apontando para as pastas de uma instalação simulada.
func New(outboundDir, backupDir, logPath string) *Fake {
	return &Fake{
		OutboundDir: outboundDir,
		BackupDir:   backupDir,
		LogPath:     logPath,
		Profile:     "PERFIL-DE-TESTE",
		FailureCode: "000401",
		Now:         time.Now,
	}
}

// Calls devolve os acionamentos observados.
//
// É o que torna o CA3 afirmável: o critério não é "o resultado foi bom", é que o cliente NÃO foi
// acionado. Uma asserção sobre o desfecho passaria mesmo se o agente transmitisse de novo e o
// segundo envio falhasse por acaso.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Run encena uma passada do cliente.
func (f *Fake) Run(_ context.Context, mode stcp.Mode, fileFilter string) (*int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, Call{Mode: mode, Filter: fileFilter})
	f.mu.Unlock()

	if mode == stcp.ModeReceive || mode == stcp.ModeBoth {
		if err := f.deliver(); err != nil {
			return nil, err
		}
	}

	if mode != stcp.ModeSend && mode != stcp.ModeBoth {
		zero := 0
		return &zero, nil
	}

	matcher, err := compileFilter(fileFilter)
	if err != nil {
		return nil, fmt.Errorf("filtro inválido %q: %w", fileFilter, err)
	}

	entries, err := os.ReadDir(f.OutboundDir)
	if err != nil {
		return nil, fmt.Errorf("ler pasta de saída simulada: %w", err)
	}

	var lines []string
	exit := 0

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// §6, p.14 — o filtro restringe o que é transmitido. Um duplo que ignorasse isto tornaria o
		// CA7 inverificável.
		if matcher != nil && !matcher.MatchString(name) {
			continue
		}

		switch f.behaviorFor(name) {
		case Crash:
			return nil, fmt.Errorf("cliente STCP simulado falhou ao executar")

		case Succeed:
			if err := os.MkdirAll(f.BackupDir, 0o750); err != nil {
				return nil, err
			}
			// §5, p.13 — sai da SAÍDA, vai para BACKUP.
			if err := os.Rename(filepath.Join(f.OutboundDir, name), filepath.Join(f.BackupDir, name)); err != nil {
				return nil, err
			}
			lines = append(lines,
				f.line(stcp.OpSendStart, stcp.ResultSuccess, name),
				f.line(stcp.OpSendEnd, stcp.ResultSuccess, name),
			)

		case Reject:
			// Permanece na pasta de saída: nada saiu.
			lines = append(lines,
				f.line(stcp.OpSendStart, stcp.ResultSuccess, name),
				f.line(stcp.OpSendEnd, f.FailureCode, name),
			)
			exit = 1

		case Vanish:
			if err := os.Remove(filepath.Join(f.OutboundDir, name)); err != nil {
				return nil, err
			}
			lines = append(lines, f.line(stcp.OpSendStart, stcp.ResultSuccess, name))
		}
	}

	if len(lines) > 0 {
		if err := f.appendLog(lines); err != nil {
			return nil, err
		}
	}
	return &exit, nil
}

func (f *Fake) behaviorFor(name string) Behavior {
	if f.Behavior == nil {
		return Succeed
	}
	return f.Behavior(name)
}

// compileFilter traduz o valor do `-f` numa expressão. Nil significa "sem filtro" — que, segundo o
// §5, quer dizer TUDO que estiver na pasta.
func compileFilter(filter string) (*regexp.Regexp, error) {
	if filter == "" {
		return nil, nil
	}
	return regexp.Compile(filter)
}

func (f *Fake) appendLog(lines []string) error {
	if err := os.MkdirAll(filepath.Dir(f.LogPath), 0o750); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer fh.Close()
	// CRLF: o arquivo é escrito por um programa Windows.
	_, err = fh.WriteString(strings.Join(lines, "\r\n") + "\r\n")
	return err
}

// line monta uma linha no layout posicional do §12 (p.30), com as dez larguras na ordem do manual.
func (f *Fake) line(op, result, fileName string) string {
	// O carimbo sai no fuso LOCAL porque é o que o cliente real faz: ele roda na máquina Windows e
	// registra a hora do relógio dela, sem indicação de zona (§12, p.30). Formatar em UTC aqui
	// deixaria o duplo mais fiel ao relógio injetado do que ao cliente que ele encena — e esconderia
	// justamente o erro de fuso que a correlação por janela de tempo pode cometer.
	now := f.Now().In(time.Local)
	var b strings.Builder
	b.WriteString(now.Format("20060102150405")) // 1 · 14 · N
	b.WriteString(op)                           // 2 ·  4 · N
	b.WriteString(pad(f.Profile, 30))           // 3 · 30 · X
	b.WriteString(pad("STCPCLT", 16))           // 4 · 16 · X
	b.WriteString(pad("00001234", 8))           // 5 ·  8 · X
	b.WriteString(pad("00005678", 8))           // 6 ·  8 · X
	b.WriteString(padNum(result, 6))            // 7 ·  6 · N
	b.WriteString(padNum("240", 12))            // 8 · 12 · N
	b.WriteString(pad(fileName, 256))           // 9 · 256 · X
	b.WriteString(pad("", 128))                 // 10 · 128 · X
	return b.String()
}

// pad alinha à esquerda com espaços, como campo alfanumérico.
func pad(s string, width int) string {
	if len(s) > width {
		return s[:width]
	}
	return s + strings.Repeat(" ", width-len(s))
}

// padNum alinha à direita com zeros, como campo numérico.
func padNum(s string, width int) string {
	if len(s) > width {
		return s[len(s)-width:]
	}
	return strings.Repeat("0", width-len(s)) + s
}

// deliver encena o banco entregando arquivos na pasta de ENTRADA.
//
// §5, p.13 — o que é recebido fica na pasta de entrada configurada no perfil. O duplo escreve o
// arquivo e, quando a entrega é logada, deixa o par de linhas de recepção (§12, p.30).
func (f *Fake) deliver() error {
	if len(f.Incoming) == 0 {
		return nil
	}
	if f.InboundDir == "" {
		return fmt.Errorf("pasta de entrada simulada não configurada")
	}
	if err := os.MkdirAll(f.InboundDir, 0o750); err != nil {
		return err
	}

	var lines []string
	for _, in := range f.Incoming {
		if err := os.WriteFile(filepath.Join(f.InboundDir, in.Name), in.Content, 0o640); err != nil {
			return err
		}
		if in.Logged {
			lines = append(lines,
				f.line(stcp.OpReceiveStart, stcp.ResultSuccess, in.Name),
				f.line(stcp.OpReceiveEnd, stcp.ResultSuccess, in.Name),
			)
		}
	}
	// Entregar uma vez só: o cliente real não reentrega o que já entregou, e um duplo que
	// reentregasse a cada acionamento esconderia o defeito de o agente não tirar o arquivo da pasta.
	f.Incoming = nil

	if len(lines) > 0 {
		return f.appendLog(lines)
	}
	return nil
}
