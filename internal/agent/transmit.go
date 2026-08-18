// Package agent contém o ciclo — a ordem em que as coisas acontecem, que é onde mora a correção
// deste componente.
//
// A ordem NÃO é preferência de estilo. Ela é a diferença entre "o pagamento atrasou" e "o pagamento
// saiu duas vezes":
//
//  1. gravar a intenção      ← durável, antes de qualquer coisa tocar a pasta de SAÍDA
//  2. depositar na SAÍDA     ← a partir daqui o cliente pode enviar a qualquer momento (§5, p.13)
//  3. acionar o cliente
//  4. ler a evidência física ← sumiu da SAÍDA e apareceu em BACKUP? (ADR-0061 §2)
//  5. registrar o desfecho
//  6. publicar o status
//  7. mover o objeto
//
// Inverter 1 e 2 abriria a janela que este componente existe para fechar: uma queda entre depositar
// e registrar deixaria um arquivo na fila do banco sem que o agente soubesse — e o próximo ciclo,
// vendo o bucket intacto, depositaria de novo.
package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// Config reúne o que o ciclo precisa decidir.
type Config struct {
	Prefixes bucket.Prefixes
	// NamePattern é a trava do CA7: só transmite arquivo cujo nome case com o padrão nosso.
	//
	// Ela é dupla de propósito. Aqui ela decide SE o arquivo entra no ciclo; no acionamento do
	// cliente, um filtro derivado do nome exato (`-f`, §6 p.14) impede que o cliente envie qualquer
	// outra coisa que esteja na pasta. A primeira é nossa; a segunda é do cliente. Confiar só na
	// nossa deixaria um arquivo largado na pasta ser enviado por uma execução manual.
	NamePattern *regexp.Regexp
	// Clock existe para que o teste controle os carimbos. Em produção é `time.Now`.
	Clock func() time.Time
}

// Validate recusa configuração incompleta no boot.
func (c Config) Validate() error {
	if err := c.Prefixes.Validate(); err != nil {
		return err
	}
	if c.NamePattern == nil {
		return errors.New("padrão de nome de remessa não configurado")
	}
	if c.Clock == nil {
		return errors.New("relógio não configurado")
	}
	return nil
}

// Agent executa os ciclos.
type Agent struct {
	store  bucket.Store
	led    ledger.Ledger
	client stcp.Client
	sp     spool.Spool
	cfg    Config
}

// New monta o agente.
func New(store bucket.Store, led ledger.Ledger, client stcp.Client, sp spool.Spool, cfg Config) (*Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Agent{store: store, led: led, client: client, sp: sp, cfg: cfg}, nil
}

// Outcome é o que aconteceu com UM objeto. `ClientInvoked` existe para que o critério de aceite de
// idempotência seja afirmável: o que ele exige não é que o resultado seja bom, é que o cliente NÃO
// tenha sido acionado.
type Outcome struct {
	Key           string
	FileName      string
	Situation     envelope.Situation
	ClientInvoked bool
	StatusKey     string
	MovedTo       string
	Err           error
}

// Summary reúne o ciclo inteiro.
type Summary struct {
	Outcomes []Outcome
}

// Errs devolve os erros acumulados. Um objeto problemático não interrompe os demais: numa fila de
// pagamentos, deixar de processar os arquivos bons por causa de um ruim é o comportamento errado.
func (s Summary) Errs() []error {
	var out []error
	for _, o := range s.Outcomes {
		if o.Err != nil {
			out = append(out, o.Err)
		}
	}
	return out
}

// TransmitCycle roda uma passada sobre o prefixo de saída.
func (a *Agent) TransmitCycle(ctx context.Context) (Summary, error) {
	keys, err := a.store.List(ctx, a.cfg.Prefixes.Outbound)
	if err != nil {
		// Falha ao LISTAR aborta o ciclo, ao contrário de falha num objeto: sem a listagem não se
		// sabe o que existe, e prosseguir seria operar sobre um mundo imaginário.
		return Summary{}, fmt.Errorf("listar prefixo de saída: %w", err)
	}

	summary := Summary{Outcomes: make([]Outcome, 0, len(keys))}
	for _, key := range keys {
		summary.Outcomes = append(summary.Outcomes, a.handle(ctx, key))
	}
	return summary, nil
}

func (a *Agent) handle(ctx context.Context, key string) Outcome {
	name := bucket.NameOf(key)
	out := Outcome{Key: key, FileName: name}

	// Nome que não produz chave segura não gera status: a chave do status DERIVA do nome, e
	// publicar com um nome sanitizado escreveria um desfecho sob uma chave que não corresponde a
	// remessa nenhuma. Segrega e deixa visível pela localização.
	if err := envelope.ValidName(name); err != nil {
		out.Err = fmt.Errorf("nome inválido em %q: %w", key, err)
		out.Situation = envelope.Review
		a.segregate(ctx, &out)
		return out
	}

	// CA7 — o filtro de nomenclatura. Um arquivo que não é nosso não vira remessa por engano.
	if !a.cfg.NamePattern.MatchString(name) {
		return a.publishAndSegregate(ctx, out, envelope.Failed,
			fmt.Sprintf("nome %q não casa com o padrão de remessa; nada foi transmitido", name), nil, nil)
	}

	entry, found, err := a.led.Lookup(name)
	if err != nil {
		// Sem saber o que o registro diz, NÃO se transmite e NÃO se move: o objeto fica na fila e a
		// próxima passada tenta de novo. Mover agora esconderia o problema; transmitir seria
		// apostar contra a única informação que impede pagamento duplicado.
		out.Err = err
		return out
	}

	if found {
		switch entry.Estado {
		case ledger.StateDone:
			return a.handleDuplicate(ctx, out, entry)
		case ledger.StateIntent:
			return a.handleInterrupted(ctx, out)
		}
	}

	return a.transmit(ctx, out, key)
}

// handleDuplicate cobre o CA3: nome já processado, cliente NÃO acionado.
//
// A situação publicada é a REGISTRADA no desfecho original, não um valor fixo — é a informação útil
// para quem investiga ("este nome já saiu", ou "este nome já falhou"). Que a tentativa repetida não
// conta como transmissão o consumidor sabe pela CHAVE, que é distinta de propósito.
func (a *Agent) handleDuplicate(ctx context.Context, out Outcome, entry ledger.Entry) Outcome {
	now := a.cfg.Clock()
	situation := envelope.Situation(entry.Situacao)
	if situation == "" {
		situation = envelope.Review
	}

	detail := fmt.Sprintf(
		"nome já processado em %s com situação %q; o cliente STCP não foi acionado",
		entry.ConcluidoEm, entry.Situacao,
	)

	env := envelope.New(out.FileName, now, situation, detail, nil, nil)
	out.Situation = situation
	out.StatusKey = envelope.DuplicateKey(out.FileName, now)
	a.publish(ctx, &out, env, out.StatusKey)

	// Segrega em vez de deixar na fila: um objeto que reaparece a cada passada produziria um status
	// de duplicado a cada cinco minutos, e o ruído esconderia o caso real. Ir para falhas o coloca
	// onde a revisão humana já olha.
	a.segregate(ctx, &out)
	return out
}

// handleInterrupted cobre o CA4: intenção gravada, desfecho desconhecido.
//
// NUNCA retransmite. O arquivo pode ter saído — e em pagamento, na dúvida, erra-se para menos.
func (a *Agent) handleInterrupted(ctx context.Context, out Outcome) Outcome {
	now := a.cfg.Clock()
	detail := "execução anterior foi interrompida após gravar a intenção e antes de registrar o desfecho; " +
		"o arquivo pode ter sido transmitido e NÃO é retransmitido automaticamente — exige conferência humana"

	logLines := a.transferLogFor(out.FileName)
	env := envelope.New(out.FileName, now, envelope.Review, detail, nil, logLines)

	out.Situation = envelope.Review
	out.StatusKey = envelope.Key(out.FileName)
	a.publish(ctx, &out, env, out.StatusKey)

	// Conclui o registro para que a reaparição do mesmo nome seja tratada como duplicado, e não
	// gere um segundo alarme de revisão. Reabrir o caso depois da conferência é ato humano
	// explícito: apagar o registro na máquina.
	if err := a.led.RecordDone(out.FileName, string(envelope.Review), now); err != nil {
		out.Err = errors.Join(out.Err, err)
	}

	a.segregate(ctx, &out)
	return out
}

// transmit é o caminho feliz — e o único que aciona o cliente.
func (a *Agent) transmit(ctx context.Context, out Outcome, key string) Outcome {
	content, err := a.store.Get(ctx, key)
	if err != nil {
		// Objeto sumiu entre listar e ler: outra execução já o tratou. Não é erro nosso e não gera
		// status — gerar um diria que houve tentativa onde não houve.
		if errors.Is(err, bucket.ErrNotFound) {
			out.Err = fmt.Errorf("objeto %q desapareceu antes da leitura: %w", key, err)
			return out
		}
		out.Err = fmt.Errorf("ler objeto %q: %w", key, err)
		return out
	}

	now := a.cfg.Clock()

	// PASSO 1 — a intenção, durável, antes de a pasta de SAÍDA ser tocada.
	if err := a.led.RecordIntent(out.FileName, now); err != nil {
		// Sem intenção gravada não se transmite. O objeto fica na fila para a próxima passada.
		out.Err = fmt.Errorf("gravar intenção de %q: %w", out.FileName, err)
		return out
	}

	// PASSO 2 — depositar. A partir daqui o arquivo está na fila do cliente (§5, p.13).
	if err := a.sp.Place(out.FileName, content); err != nil {
		// A intenção JÁ está gravada e permanece: o depósito pode ter acontecido parcialmente, e
		// tratar como "não tentou" liberaria uma segunda tentativa sobre um estado desconhecido.
		// O próximo ciclo verá `intent` e mandará para revisão — que é o desfecho correto.
		out.Err = fmt.Errorf("depositar %q na pasta de saída: %w", out.FileName, err)
		return out
	}

	// PASSO 3 — acionar.
	exitCode, runErr := a.client.Run(ctx, stcp.ModeSend, FileFilter(out.FileName))
	out.ClientInvoked = true

	// PASSO 4 — a evidência física decide.
	situation, detail := a.verdict(out.FileName, exitCode, runErr)

	logLines := a.transferLogFor(out.FileName)
	env := envelope.New(out.FileName, a.cfg.Clock(), situation, detail, exitCode, logLines)

	// PASSO 5 — registrar antes de publicar. Uma queda entre os dois deixa o registro concluído e o
	// status ausente: o próximo ciclo trata como duplicado e não retransmite. A ordem inversa
	// publicaria um desfecho que o registro não conhece, e o arquivo voltaria à fila.
	if err := a.led.RecordDone(out.FileName, string(situation), a.cfg.Clock()); err != nil {
		out.Err = errors.Join(out.Err, fmt.Errorf("registrar desfecho de %q: %w", out.FileName, err))
	}

	out.Situation = situation
	out.StatusKey = envelope.Key(out.FileName)

	// PASSO 6 — publicar.
	a.publish(ctx, &out, env, out.StatusKey)

	// PASSO 7 — mover. Transmitido vai para processados; todo o resto aguarda revisão humana, e
	// NADA é retentado automaticamente (CA2).
	if situation == envelope.Transmitted {
		a.moveTo(ctx, &out, a.cfg.Prefixes.Processed)
	} else {
		a.segregate(ctx, &out)
	}
	return out
}

// verdict traduz evidência física em situação.
//
// O código de saída NÃO decide. Ele entra no envelope como diagnóstico, e a razão está no manual: a
// v5.3 documenta o resultado no log (§12), nunca o significado do código de saída do executável.
func (a *Agent) verdict(fileName string, exitCode *int, runErr error) (envelope.Situation, string) {
	stillQueued, errOut := a.sp.InOutbound(fileName)
	backedUp, errBak := a.sp.InBackup(fileName)

	if errOut != nil || errBak != nil {
		// Sem conseguir ler a evidência, o desfecho é desconhecido — e desconhecido vai para
		// revisão, nunca para falha. Falha convidaria alguém a reenviar um arquivo que talvez tenha
		// saído.
		return envelope.Review, fmt.Sprintf(
			"não foi possível conferir a evidência física de %q (saída: %v; backup: %v); desfecho desconhecido",
			fileName, errOut, errBak)
	}

	switch {
	case !stillQueued && backedUp:
		detail := "arquivo saiu da pasta de saída e apareceu em backup"
		if runErr != nil {
			// A evidência física é mais forte que o erro de execução: se o arquivo saiu, ele saiu.
			// O erro fica registrado no detalhe para quem investigar.
			detail += fmt.Sprintf("; o acionamento reportou erro (%v), mas a evidência física prevalece", runErr)
		}
		return envelope.Transmitted, detail

	case stillQueued:
		detail := "arquivo permanece na pasta de saída; nada foi transmitido"
		if runErr != nil {
			detail += fmt.Sprintf("; falha ao acionar o cliente: %v", runErr)
		} else if exitCode != nil {
			detail += fmt.Sprintf("; código de saída do cliente: %d", *exitCode)
		}
		return envelope.Failed, detail

	default:
		// Sumiu da saída e não está em backup. Pode ter saído e o backup ter falhado, pode ter sido
		// removido por outra coisa. Ambíguo é revisão, sempre.
		return envelope.Review, "arquivo saiu da pasta de saída mas não apareceu em backup; " +
			"desfecho ambíguo, NÃO retransmitir sem conferência"
	}
}

// transferLogFor lê e filtra o log daquele arquivo.
//
// As linhas vão CRUAS para o envelope. A decodificação existe para escolher quais linhas importam,
// nunca para substituí-las: se um offset estiver errado, quem investiga ainda tem o texto original.
func (a *Agent) transferLogFor(fileName string) []string {
	raw, err := a.sp.ReadTransferLog()
	if err != nil || raw == "" {
		return nil
	}
	return stcp.RawLines(stcp.FilterByFile(stcp.ParseLog(raw), fileName))
}

// publishAndSegregate cobre os desfechos em que o ciclo recusa o arquivo ANTES de qualquer
// tentativa: publica o motivo e tira o objeto da fila.
//
// Não toca no registro de intenção de propósito. O registro guarda tentativas de transmissão, e
// aqui não houve nenhuma — gravar um desfecho para um arquivo que nunca foi nosso poluiria
// justamente o mecanismo que decide se um nome pode ser transmitido.
func (a *Agent) publishAndSegregate(
	ctx context.Context,
	out Outcome,
	situation envelope.Situation,
	detail string,
	exitCode *int,
	logLines []string,
) Outcome {
	env := envelope.New(out.FileName, a.cfg.Clock(), situation, detail, exitCode, logLines)
	out.Situation = situation
	out.StatusKey = envelope.Key(out.FileName)
	a.publish(ctx, &out, env, out.StatusKey)
	a.segregate(ctx, &out)
	return out
}

func (a *Agent) publish(ctx context.Context, out *Outcome, env envelope.Envelope, key string) {
	body, err := envelope.Marshal(env)
	if err != nil {
		out.Err = errors.Join(out.Err, err)
		return
	}
	if err := a.store.Put(ctx, key, body); err != nil {
		out.Err = errors.Join(out.Err, fmt.Errorf("publicar status em %q: %w", key, err))
	}
}

func (a *Agent) segregate(ctx context.Context, out *Outcome) {
	a.moveTo(ctx, out, a.cfg.Prefixes.Failed)
}

func (a *Agent) moveTo(ctx context.Context, out *Outcome, prefix string) {
	dest := prefix + out.FileName
	if err := a.store.Move(ctx, out.Key, dest); err != nil {
		out.Err = errors.Join(out.Err, fmt.Errorf("mover %q para %q: %w", out.Key, dest, err))
		return
	}
	out.MovedTo = dest
}

// FileFilter monta o valor do parâmetro `-f` para um nome exato (§6, p.14).
//
// ⚠️ O manual diz que o parâmetro aceita expressão regular, mas NÃO declara o dialeto. O escape
// aqui cobre os metacaracteres comuns a praticamente todo dialeto, e as âncoras exigem casamento
// total. Confirmar o dialeto contra a instalação é pendência aberta — até lá, a trava real contra
// enviar arquivo indevido é dupla: este filtro e a checagem de padrão antes de depositar.
func FileFilter(fileName string) string {
	var b strings.Builder
	b.WriteByte('^')
	for _, r := range fileName {
		if strings.ContainsRune(`.\+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('$')
	return b.String()
}
