package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp/stcpfake"
)

// Nome fictício, no formato que a infra sugeriu (ADR-0061, "O que continua em aberto" §1). O
// convênio real não entra em teste nem em código: este repositório é público.
const remittanceName = "PAG_000000.20260818120000_0001.REM"

// O padrão que decide o que é remessa nossa (CA7).
var namePattern = regexp.MustCompile(`^PAG_\d+\.\d+_\d+\.REM$`)

const remittanceContent = "0371234...CONTEUDO CNAB FICTICIO..."

// harness monta uma instalação simulada completa: bucket em memória, registro em disco real (para
// exercitar O_EXCL e fsync de verdade), pastas do cliente em disco real e o duplo do STCPCLT.
//
// O registro e as pastas são REAIS de propósito. Um duplo do registro tornaria o teste de morte no
// meio uma tautologia — ele estaria verificando o duplo, não o mecanismo que protege o pagamento.
type harness struct {
	t           *testing.T
	store       *bucket.Memory
	led         *ledger.FileLedger
	fake        *stcpfake.Fake
	ag          *agent.Agent
	prefixes    bucket.Prefixes
	outboundDir string
	backupDir   string
	logDir      string
	now         time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	root := t.TempDir()
	outboundDir := filepath.Join(root, "stcp", "SAIDA")
	backupDir := filepath.Join(root, "stcp", "BACKUP")
	logDir := filepath.Join(root, "stcp", "LOG")
	ledgerDir := filepath.Join(root, "ledger")

	for _, d := range []string{outboundDir, backupDir, logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("preparar %s: %v", d, err)
		}
	}

	led, err := ledger.NewFileLedger(ledgerDir)
	if err != nil {
		t.Fatalf("registro: %v", err)
	}

	sp, err := spool.NewDir(spool.Config{
		OutboundDir:     outboundDir,
		BackupDir:       backupDir,
		LogDir:          logDir,
		TransferLogGlob: "*.LOG",
	})
	if err != nil {
		t.Fatalf("pastas do cliente: %v", err)
	}

	fake := stcpfake.New(outboundDir, backupDir, filepath.Join(logDir, "20260818.LOG"))
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	fake.Now = func() time.Time { return now }

	store := bucket.NewMemory()
	prefixes := bucket.DefaultPrefixes()

	ag, err := agent.New(store, led, fake, sp, agent.Config{
		Prefixes:    prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}

	return &harness{
		t: t, store: store, led: led, fake: fake, ag: ag, prefixes: prefixes,
		outboundDir: outboundDir, backupDir: backupDir, logDir: logDir, now: now,
	}
}

func (h *harness) queue(name, content string) {
	h.t.Helper()
	h.store.Seed(h.prefixes.Outbound+name, []byte(content))
}

func (h *harness) run() agent.Summary {
	h.t.Helper()
	sum, err := h.ag.TransmitCycle(context.Background())
	if err != nil {
		h.t.Fatalf("ciclo abortou: %v", err)
	}
	return sum
}

func (h *harness) statusFor(key string) envelope.Envelope {
	h.t.Helper()
	raw, err := h.store.Get(context.Background(), key)
	if err != nil {
		h.t.Fatalf("status ausente em %q; chaves existentes: %v", key, h.store.Keys())
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("status em %q ilegível: %v", key, err)
	}
	return env
}

func (h *harness) hasObject(key string) bool {
	_, err := h.store.Get(context.Background(), key)
	return err == nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CA1 — Transmissão
// ─────────────────────────────────────────────────────────────────────────────

func TestCA1_TransmissaoPublicaStatusEMoveParaProcessados(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}
	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("ciclo acumulou erros: %v", errs)
	}

	got := sum.Outcomes[0]
	if got.Situation != envelope.Transmitted {
		t.Errorf("situação = %q, esperava %q", got.Situation, envelope.Transmitted)
	}
	if !got.ClientInvoked {
		t.Error("o cliente STCP deveria ter sido acionado")
	}

	// O desfecho aparece no status, correlacionado POR NOME.
	env := h.statusFor(envelope.Key(remittanceName))
	if env.Arquivo != remittanceName {
		t.Errorf("arquivo no envelope = %q, esperava %q", env.Arquivo, remittanceName)
	}
	if env.Situacao != envelope.Transmitted {
		t.Errorf("situação no envelope = %q, esperava %q", env.Situacao, envelope.Transmitted)
	}

	// O objeto sai da fila e vai para processados — a localização É o estado (ADR-0061 §1).
	if h.hasObject(h.prefixes.Outbound + remittanceName) {
		t.Error("o objeto continua no prefixo de saída")
	}
	if !h.hasObject(h.prefixes.Processed + remittanceName) {
		t.Errorf("o objeto não chegou a processados; chaves: %v", h.store.Keys())
	}

	// A evidência física que sustentou o veredito.
	if _, err := os.Stat(filepath.Join(h.backupDir, remittanceName)); err != nil {
		t.Errorf("o arquivo deveria estar em BACKUP: %v", err)
	}
}

func TestCA1_EnvelopeCarregaAsLinhasCruasDoLog(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)

	h.run()

	env := h.statusFor(envelope.Key(remittanceName))
	if len(env.LogTransferencia) == 0 {
		t.Fatal("o envelope deveria carregar as linhas do log de transferência")
	}
	for _, line := range env.LogTransferencia {
		if !strings.Contains(line, remittanceName) {
			t.Errorf("linha de log não corresponde ao arquivo: %q", line)
		}
	}
	// Fim de transmissão com resultado de sucesso (§12, p.30).
	found := false
	for _, line := range env.LogTransferencia {
		r := stcp.ParseLine(line)
		if r.Op == stcp.OpSendEnd && r.Succeeded() {
			found = true
		}
	}
	if !found {
		t.Error("esperava linha de fim de transmissão com resultado de sucesso")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA2 — Falha de transporte: vai para falhas, e NADA é retentado automaticamente
// ─────────────────────────────────────────────────────────────────────────────

func TestCA2_RecusaVaiParaFalhasComCodigoENaoRetenta(t *testing.T) {
	h := newHarness(t)
	h.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Reject }
	h.fake.FailureCode = "000401" // §11, p.24 — nome inválido ou filtro de nomenclatura
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Failed {
		t.Errorf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Failed)
	}
	if !h.hasObject(h.prefixes.Failed + remittanceName) {
		t.Errorf("o objeto deveria estar em falhas; chaves: %v", h.store.Keys())
	}

	env := h.statusFor(envelope.Key(remittanceName))
	if env.Situacao != envelope.Failed {
		t.Errorf("situação no envelope = %q, esperava %q", env.Situacao, envelope.Failed)
	}
	// O código do erro precisa chegar a quem investiga — sem ele, "falhou" não diz o que fazer.
	joined := strings.Join(env.LogTransferencia, "\n")
	if !strings.Contains(joined, "000401") {
		t.Errorf("o log do envelope deveria conter o código de erro; veio: %q", joined)
	}

	// E a parte que mais importa: nada é retentado. Uma segunda passada não aciona o cliente.
	callsBefore := len(h.fake.Calls())
	h.run()
	if len(h.fake.Calls()) != callsBefore {
		t.Errorf("o cliente foi acionado de novo: %d → %d chamadas", callsBefore, len(h.fake.Calls()))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA3 — Idempotência: nome já processado NÃO aciona o cliente
// ─────────────────────────────────────────────────────────────────────────────

func TestCA3_NomeJaProcessadoNaoAcionaOCliente(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)
	h.run()

	callsAfterFirst := len(h.fake.Calls())
	if callsAfterFirst == 0 {
		t.Fatal("a primeira passada deveria ter acionado o cliente")
	}

	// O mesmo nome reaparece na fila — é o cenário do CA3.
	h.queue(remittanceName, remittanceContent)
	sum := h.run()

	if len(h.fake.Calls()) != callsAfterFirst {
		t.Fatalf("o cliente STCP foi acionado na segunda vez: %d → %d chamadas",
			callsAfterFirst, len(h.fake.Calls()))
	}
	if sum.Outcomes[0].ClientInvoked {
		t.Error("o desfecho não deveria registrar acionamento do cliente")
	}
}

func TestCA3_DuplicadoPublicaSobChaveDistintaSemApagarOOriginal(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)
	h.run()

	original := h.statusFor(envelope.Key(remittanceName))

	h.queue(remittanceName, remittanceContent)
	h.run()

	// O status original continua dizendo que a remessa saiu. Se o duplicado o sobrescrevesse, uma
	// remessa JÁ transmitida passaria a constar como não transmitida — e alguém a reenviaria.
	stillOriginal := h.statusFor(envelope.Key(remittanceName))
	if stillOriginal.Situacao != original.Situacao {
		t.Errorf("o status original foi sobrescrito: %q → %q", original.Situacao, stillOriginal.Situacao)
	}

	dupKey := envelope.DuplicateKey(remittanceName, h.now)
	dup := h.statusFor(dupKey)
	if dup.ExitCode != nil {
		t.Errorf("exitCode do duplicado = %v, esperava null (o cliente não executou)", *dup.ExitCode)
	}
	if !strings.Contains(dup.Detalhe, "não foi acionado") {
		t.Errorf("o detalhe deveria dizer que o cliente não foi acionado; veio: %q", dup.Detalhe)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA4 — Morte no meio: intenção gravada, desfecho desconhecido → revisão humana
// ─────────────────────────────────────────────────────────────────────────────

func TestCA4_ExecucaoInterrompidaVaiParaRevisaoENuncaRetransmite(t *testing.T) {
	h := newHarness(t)

	// Encena exatamente a morte no meio: a intenção foi gravada e o desfecho nunca foi registrado.
	// É o estado que o disco guardaria se o processo tivesse sido morto entre os passos 1 e 5.
	if err := h.led.RecordIntent(remittanceName, h.now); err != nil {
		t.Fatalf("gravar intenção: %v", err)
	}
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if len(h.fake.Calls()) != 0 {
		t.Fatalf("o cliente NÃO podia ser acionado; houve %d chamada(s)", len(h.fake.Calls()))
	}
	if sum.Outcomes[0].Situation != envelope.Review {
		t.Errorf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Review)
	}
	if !h.hasObject(h.prefixes.Failed + remittanceName) {
		t.Errorf("o objeto deveria estar segregado em falhas; chaves: %v", h.store.Keys())
	}

	env := h.statusFor(envelope.Key(remittanceName))
	if env.Situacao != envelope.Review {
		t.Errorf("situação no envelope = %q, esperava %q", env.Situacao, envelope.Review)
	}
	if !strings.Contains(env.Detalhe, "conferência humana") {
		t.Errorf("o detalhe deveria encaminhar para conferência humana; veio: %q", env.Detalhe)
	}
}

func TestCA4_DesfechoAmbiguoNaoViraSucesso(t *testing.T) {
	h := newHarness(t)
	// O arquivo some da SAÍDA sem aparecer em BACKUP: sem evidência física, o desfecho é ambíguo.
	h.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Vanish }
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Review {
		t.Errorf("situação = %q, esperava %q — ambíguo nunca é sucesso",
			sum.Outcomes[0].Situation, envelope.Review)
	}
	if h.hasObject(h.prefixes.Processed + remittanceName) {
		t.Error("desfecho ambíguo não pode ir para processados")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA7 — Filtro de nome
// ─────────────────────────────────────────────────────────────────────────────

func TestCA7_NomeForaDoPadraoNaoEhTransmitido(t *testing.T) {
	h := newHarness(t)
	h.queue("QUALQUER-COISA.txt", "conteúdo alheio")

	sum := h.run()

	if len(h.fake.Calls()) != 0 {
		t.Fatalf("o cliente não podia ser acionado para arquivo fora do padrão; %d chamada(s)",
			len(h.fake.Calls()))
	}
	if sum.Outcomes[0].Situation != envelope.Failed {
		t.Errorf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Failed)
	}
	if !h.hasObject(h.prefixes.Failed + "QUALQUER-COISA.txt") {
		t.Errorf("o objeto deveria estar em falhas; chaves: %v", h.store.Keys())
	}
}

// O teste que vale mais: um arquivo estranho JÁ está na pasta de SAÍDA quando o ciclo roda.
//
// Segundo o §5 (p.13), tudo que estiver na pasta é enviado. Sem o filtro do `-f`, acionar o cliente
// para transmitir a nossa remessa transmitiria o intruso junto — e um arquivo indevido chegando ao
// banco pelo nosso perfil é exatamente o que o CA7 existe para impedir.
func TestCA7_ArquivoIntrusoNaPastaDeSaidaNaoEhTransmitidoJunto(t *testing.T) {
	h := newHarness(t)

	intruso := filepath.Join(h.outboundDir, "ARQUIVO-QUE-NAO-E-NOSSO.REM")
	if err := os.WriteFile(intruso, []byte("não deveria sair daqui"), 0o640); err != nil {
		t.Fatalf("plantar intruso: %v", err)
	}

	h.queue(remittanceName, remittanceContent)
	h.run()

	if _, err := os.Stat(intruso); err != nil {
		t.Errorf("o arquivo intruso saiu da pasta de saída — foi transmitido: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.backupDir, "ARQUIVO-QUE-NAO-E-NOSSO.REM")); err == nil {
		t.Error("o arquivo intruso apareceu em BACKUP — foi transmitido")
	}

	// E a nossa remessa saiu normalmente.
	if _, err := os.Stat(filepath.Join(h.backupDir, remittanceName)); err != nil {
		t.Errorf("a nossa remessa deveria ter sido transmitida: %v", err)
	}
}

func TestCA7_FiltroPassadoAoClienteAncoraONomeExato(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)
	h.run()

	calls := h.fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("esperava 1 acionamento, veio %d", len(calls))
	}
	if calls[0].Mode != stcp.ModeSend {
		t.Errorf("modo = %q, esperava %q — o ciclo de saída não recebe", calls[0].Mode, stcp.ModeSend)
	}
	filter := calls[0].Filter
	if !strings.HasPrefix(filter, "^") || !strings.HasSuffix(filter, "$") {
		t.Errorf("o filtro precisa ancorar o nome inteiro; veio %q", filter)
	}
	// O ponto do nome precisa estar escapado: sem escape, `.` casa qualquer caractere, e o filtro
	// que deveria restringir passaria a aceitar arquivos vizinhos.
	if strings.Contains(filter, `_000000.2026`) {
		t.Errorf("o ponto do nome não foi escapado no filtro: %q", filter)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Fronteira: nome que produziria chave insegura
// ─────────────────────────────────────────────────────────────────────────────

func TestNomeComTravessiaNaoEhTransmitidoNemPublicaStatus(t *testing.T) {
	h := newHarness(t)
	// A chave carrega travessia; `NameOf` extrai o último segmento, mas a validação recusa antes de
	// qualquer coisa acontecer.
	h.store.Seed(h.prefixes.Outbound+"..%2F..%2Fetc%2Fpasswd", []byte("x"))

	sum := h.run()

	if len(h.fake.Calls()) != 0 {
		t.Fatalf("o cliente não podia ser acionado; %d chamada(s)", len(h.fake.Calls()))
	}
	if sum.Outcomes[0].Err == nil {
		t.Error("esperava erro registrado para nome inválido")
	}
}
