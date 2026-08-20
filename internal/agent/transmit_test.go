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
	sp          spool.Spool
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

	// Registro de pendências no harness principal para que TODA a suíte rode o caminho de produção:
	// um harness que publicasse direto verificaria um agente que não existe na instalação.
	pending, err := ledger.NewFilePendingEnvelopes(filepath.Join(root, "ledger", "pendentes"))
	if err != nil {
		t.Fatalf("registro de pendências: %v", err)
	}

	ag, err := agent.New(store, led, fake, sp, agent.Config{
		Prefixes:    prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return now },
	}, agent.WithPendingEnvelopes(pending))
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}

	return &harness{
		t: t, store: store, led: led, sp: sp, fake: fake, ag: ag, prefixes: prefixes,
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

// O código do banco passa a chegar ao campo `detalhe`, e não só embutido na linha crua.
//
// Antes disto, o código só existia no log posicional dentro de `logTransferencia` — legível por
// quem souber os offsets do §12, e por mais ninguém. O core-api mediu e confirmou que NÃO lê
// `logTransferencia` por máquina; `detalhe` é o campo que ele persiste e devolve na borda. Campo
// próprio no envelope mudaria o contrato e ficou registrado como condicional.
func TestCA2_CodigoDoBancoChegaAoDetalheENaoSoAoLogCru(t *testing.T) {
	h := newHarness(t)
	h.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Reject }
	h.fake.FailureCode = "000401" // §11, p.24 — nome inválido ou filtro de nomenclatura
	h.queue(remittanceName, remittanceContent)

	h.run()

	env := h.statusFor(envelope.Key(remittanceName))
	if !strings.Contains(env.Detalhe, "000401") {
		t.Errorf("o código do banco precisa chegar ao detalhe; veio: %q", env.Detalhe)
	}
	if !strings.Contains(env.Detalhe, "§11") {
		t.Errorf("o detalhe precisa dizer contra qual tabela o código vale; veio: %q", env.Detalhe)
	}
	// E o que o detalhe já dizia continua lá: o código ACRESCENTA, nunca substitui.
	if !strings.Contains(env.Detalhe, "arquivo permanece na pasta de saída") {
		t.Errorf("o código comeu o texto do desfecho; veio: %q", env.Detalhe)
	}
	// O veredito não muda: quem decide é a evidência física, não o log.
	if env.Situacao != envelope.Failed {
		t.Errorf("situação = %q, esperava %q — o código do banco é diagnóstico, não veredito",
			env.Situacao, envelope.Failed)
	}
}

func TestCA1_TransmissaoBemSucedidaNaoInventaCodigoDeFalha(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)

	h.run()

	env := h.statusFor(envelope.Key(remittanceName))
	if strings.Contains(env.Detalhe, "§11") {
		t.Errorf("transmissão bem-sucedida não pode carregar código de falha; veio: %q", env.Detalhe)
	}
	if env.Detalhe != "arquivo saiu da pasta de saída e apareceu em backup" {
		t.Errorf("o detalhe do caminho feliz mudou: %q", env.Detalhe)
	}
}

func TestCA4_SemLinhaDeFimDeTransmissaoODetalheNaoAfirmaCodigoNenhum(t *testing.T) {
	// `Vanish` deixa só a linha de INÍCIO: o arquivo sai da SAÍDA e não aparece em BACKUP. Não há
	// código de falha para reportar — e a ausência dele NÃO é prova de que não houve falha, é
	// ausência de evidência. O detalhe precisa continuar dizendo só o que sabe.
	h := newHarness(t)
	h.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Vanish }
	h.queue(remittanceName, remittanceContent)

	h.run()

	env := h.statusFor(envelope.Key(remittanceName))
	if strings.Contains(env.Detalhe, "§11") {
		t.Errorf("sem linha de fim de transmissão não há código a afirmar; veio: %q", env.Detalhe)
	}
	if env.Situacao != envelope.Review {
		t.Errorf("situação = %q, esperava %q", env.Situacao, envelope.Review)
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

// O caminho interrompido é onde o código do banco vale MAIS, e é por isso que ele tem teste próprio.
//
// Aqui ninguém sabe o que aconteceu: a intenção ficou gravada e o desfecho nunca foi registrado. Se
// o cliente chegou a escrever uma linha de fim de transmissão para aquele nome antes de o processo
// morrer, o que ela diz é a primeira informação que a conferência humana precisa — e sem isto ela
// só chegaria embutida na linha crua, que ninguém lê por máquina.
func TestCA4_InterrompidoCarregaOCodigoQueOClienteChegouAGravar(t *testing.T) {
	h := newHarness(t)

	// Encena o ciclo que MORREU: o cliente foi acionado de verdade e deixou a linha no log; o
	// processo caiu antes do passo 5. Acionar o duplo direto, e não pelo agente, é o que reproduz
	// esse estado — pelo agente o desfecho teria sido registrado, e aí não haveria interrupção.
	if err := os.WriteFile(filepath.Join(h.outboundDir, remittanceName),
		[]byte(remittanceContent), 0o600); err != nil {
		t.Fatalf("depositar na saída: %v", err)
	}
	h.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Reject }
	h.fake.FailureCode = "000403"
	if _, err := h.fake.Run(context.Background(), stcp.ModeSend, agent.FileFilter(remittanceName)); err != nil {
		t.Fatalf("encenar o acionamento do ciclo anterior: %v", err)
	}
	chamadasAntes := len(h.fake.Calls())

	if err := h.led.RecordIntent(remittanceName, h.now); err != nil {
		t.Fatalf("gravar intenção: %v", err)
	}
	h.queue(remittanceName, remittanceContent)

	h.run()

	// A garantia do CA4 continua valendo: interrompido NUNCA retransmite.
	if len(h.fake.Calls()) != chamadasAntes {
		t.Fatalf("o cliente foi acionado de novo: %d → %d", chamadasAntes, len(h.fake.Calls()))
	}

	env := h.statusFor(envelope.Key(remittanceName))
	if env.Situacao != envelope.Review {
		t.Errorf("situação = %q, esperava %q", env.Situacao, envelope.Review)
	}
	if !strings.Contains(env.Detalhe, "000403") {
		t.Errorf("o detalhe deveria carregar o código que o cliente gravou; veio: %q", env.Detalhe)
	}
	// E o encaminhamento para o humano continua na frase — o código acrescenta, não substitui.
	if !strings.Contains(env.Detalhe, "conferência humana") {
		t.Errorf("o código comeu o encaminhamento para conferência; veio: %q", env.Detalhe)
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

// ─────────────────────────────────────────────────────────────────────────────
// Trava de comprimento do nome
//
// O manual documenta um erro dedicado a nome longo (1101, §11 p.26), e o procedimento que ele
// descreve é CONDICIONAL: depende de a opção de nome longo estar habilitada na instalação e de o
// parceiro incorporá-la — nenhuma das duas verificada por medição. A trava existe para que a recusa
// aconteça DESTE lado da fronteira, antes de o arquivo entrar na fila do banco.
// ─────────────────────────────────────────────────────────────────────────────

// harnessComTeto monta o agente com a trava ligada. O resto é idêntico ao harness padrão: a trava é
// configuração do ciclo, não um caminho separado.
func harnessComTeto(t *testing.T, teto int) *harness {
	t.Helper()
	h := newHarness(t)
	ag, err := agent.New(h.store, h.led, h.fake, h.sp, agent.Config{
		Prefixes:      h.prefixes,
		NamePattern:   namePattern,
		NameMaxLength: teto,
		Clock:         func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatalf("montar agente com teto: %v", err)
	}
	h.ag = ag
	return h
}

func TestTeto_NomeLongoVaiParaFalhasSemAcionarOCliente(t *testing.T) {
	// O nome do fixture tem 34 caracteres; um teto de 26 é o que o manual documenta como base.
	h := harnessComTeto(t, 26)
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	// O que mais importa: nada chegou ao banco.
	if len(h.fake.Calls()) != 0 {
		t.Fatalf("o cliente NÃO podia ser acionado para nome longo; houve %d chamada(s)", len(h.fake.Calls()))
	}
	if sum.Outcomes[0].Situation != envelope.Failed {
		t.Errorf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Failed)
	}
	if !h.hasObject(h.prefixes.Failed + remittanceName) {
		t.Errorf("o objeto deveria estar em falhas; chaves: %v", h.store.Keys())
	}

	env := h.statusFor(envelope.Key(remittanceName))
	if !strings.Contains(env.Detalhe, agent.RefusalNameLength) {
		t.Errorf("o detalhe deveria carregar o código %q; veio: %q", agent.RefusalNameLength, env.Detalhe)
	}
	// O arquivo continua na pasta de SAÍDA? Não — ele nunca foi depositado lá. A recusa acontece
	// antes do passo 2, que é o ponto do critério: recusar antes de tocar a fila do banco.
	if _, err := os.Stat(filepath.Join(h.outboundDir, remittanceName)); err == nil {
		t.Error("o arquivo foi depositado na pasta de SAÍDA apesar da recusa")
	}
}

// O nome NÃO é truncado, e a razão não é estilo: truncar mudaria a chave de idempotência, e dois
// nomes distintos truncados para o mesmo prefixo colidiriam no registro — a segunda remessa seria
// lida como duplicado e nunca sairia.
func TestTeto_NomeLongoNaoEhTruncadoNemGravaIntencao(t *testing.T) {
	h := harnessComTeto(t, 26)
	h.queue(remittanceName, remittanceContent)
	h.run()

	if _, found, err := h.led.Lookup(remittanceName); err != nil {
		t.Fatalf("consultar registro: %v", err)
	} else if found {
		t.Error("uma recusa de nomenclatura não pode gravar registro: não houve tentativa de transmissão")
	}
	// E nada com nome truncado apareceu em lugar nenhum.
	for _, k := range h.store.Keys() {
		if nome := bucket.NameOf(k); nome != remittanceName && strings.HasPrefix(remittanceName, nome) {
			t.Errorf("apareceu um objeto com nome truncado: %q", k)
		}
	}
}

func TestTeto_NomeDentroDoLimiteSegueNormalmente(t *testing.T) {
	h := harnessComTeto(t, len(remittanceName))
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Transmitted {
		t.Errorf("nome exatamente no limite deveria passar; situação = %q", sum.Outcomes[0].Situation)
	}
}

// Sem trava configurada, nada muda para quem já roda: é o default, e é deliberado — o teto efetivo
// depende da instalação e do parceiro, e recusar por engano pararia a fila inteira.
func TestTeto_SemConfiguracaoNaoHaTrava(t *testing.T) {
	h := newHarness(t)
	h.queue(remittanceName, remittanceContent)

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Transmitted {
		t.Errorf("sem teto configurado a remessa deveria seguir; situação = %q", sum.Outcomes[0].Situation)
	}
}

// A checagem de comprimento NÃO substitui a de forma: um nome curto e fora do padrão continua
// recusado, e com um código diferente — as duas causas levam a ações diferentes.
func TestTeto_NomeCurtoForaDoPadraoContinuaRecusadoComOutroCodigo(t *testing.T) {
	h := harnessComTeto(t, 26)
	h.queue("X.txt", "conteúdo alheio")

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Failed {
		t.Errorf("situação = %q, esperava %q", sum.Outcomes[0].Situation, envelope.Failed)
	}
	env := h.statusFor(envelope.Key("X.txt"))
	if !strings.Contains(env.Detalhe, agent.RefusalNamePattern) {
		t.Errorf("o detalhe deveria carregar o código %q; veio: %q", agent.RefusalNamePattern, env.Detalhe)
	}
	if strings.Contains(env.Detalhe, agent.RefusalNameLength) {
		t.Errorf("um nome curto não pode ser recusado por comprimento; veio: %q", env.Detalhe)
	}
}

// O critério que fecha o item: quem lê o envelope precisa distinguir recusa do TRANSPORTE de recusa
// do BANCO. A primeira se conserta mudando o nome no emissor; a segunda exige olhar o convênio.
func TestTeto_EnvelopeDistingueRecusaDoTransporteDeRecusaDoBanco(t *testing.T) {
	transporte := harnessComTeto(t, 26)
	transporte.queue(remittanceName, remittanceContent)
	transporte.run()
	recusadoAqui := transporte.statusFor(envelope.Key(remittanceName))

	banco := newHarness(t)
	banco.fake.Behavior = func(string) stcpfake.Behavior { return stcpfake.Reject }
	banco.queue(remittanceName, remittanceContent)
	banco.run()
	recusadoLa := banco.statusFor(envelope.Key(remittanceName))

	// As duas são `falha` — a situação sozinha não basta, e é por isso que o critério existe.
	if recusadoAqui.Situacao != recusadoLa.Situacao {
		t.Fatalf("as duas recusas deveriam compartilhar a situação; %q vs %q",
			recusadoAqui.Situacao, recusadoLa.Situacao)
	}

	// Recusa do transporte: o cliente não rodou, então não há código de saída nem linha de log.
	if recusadoAqui.ExitCode != nil {
		t.Errorf("recusa do transporte não pode ter exitCode; veio %d", *recusadoAqui.ExitCode)
	}
	if len(recusadoAqui.LogTransferencia) != 0 {
		t.Errorf("recusa do transporte não pode ter linha de log; veio %v", recusadoAqui.LogTransferencia)
	}
	if !strings.Contains(recusadoAqui.Detalhe, "nenhuma tentativa chegou ao banco") {
		t.Errorf("o detalhe precisa dizer que nada chegou ao banco; veio: %q", recusadoAqui.Detalhe)
	}

	// Recusa do banco: o cliente rodou, e a evidência disso está no envelope.
	if recusadoLa.ExitCode == nil {
		t.Error("recusa do banco deveria trazer o código de saída do cliente")
	}
	if len(recusadoLa.LogTransferencia) == 0 {
		t.Error("recusa do banco deveria trazer as linhas do log")
	}
	if strings.Contains(recusadoLa.Detalhe, "recusa-nomenclatura") {
		t.Errorf("recusa do banco não pode carregar código de nomenclatura; veio: %q", recusadoLa.Detalhe)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Uma chave de remessa por arquivo, por mais ciclos que rodem
// ─────────────────────────────────────────────────────────────────────────────

// A trava no nível do CICLO, e não da função.
//
// O core-api resolve conflito entre dois envelopes do mesmo arquivo pela ordem LEXICOGRÁFICA da
// chave: vence o primeiro, `executadoEm` não é consultado, e o agregado recusa a segunda mudança em
// qualquer direção — inclusive promoção de falha para sucesso (medido por eles em 19/08). Isso só é
// seguro porque o agente publica UMA chave de remessa por arquivo, sempre a mesma.
//
// `TestCA3_DuplicadoPublicaSobChaveDistintaSemApagarOOriginal` já cobre que o duplicado não
// sobrescreve — mas ele BUSCA por `envelope.Key(...)`, então acompanharia a mudança se a chave
// passasse a variar: concordaria com o defeito em vez de o denunciar. Aqui as chaves são
// classificadas pela FORMA, do mesmo jeito que o consumidor faz, sem perguntar ao produtor qual é a
// chave certa.
func TestUmaUnicaChaveDeRemessaPorArquivoAposVariosCiclos(t *testing.T) {
	h := newHarness(t)

	// Três ciclos com o MESMO nome. Do segundo em diante é o caminho do reprocessamento: devolver um
	// objeto à fila com o nome já concluído, que o épico #6 registra como armadilha conhecida.
	for range 3 {
		h.queue(remittanceName, remittanceContent)
		h.run()
	}

	var remessa, duplicado, recepcao, outras []string
	for _, k := range h.store.Keys() {
		if !strings.HasPrefix(k, "status/") {
			continue
		}
		switch {
		case strings.Contains(k, ".duplicado-"):
			duplicado = append(duplicado, k)
		case strings.HasPrefix(k, "status/recepcao-"):
			recepcao = append(recepcao, k)
		case strings.HasSuffix(k, ".json"):
			remessa = append(remessa, k)
		default:
			outras = append(outras, k)
		}
	}

	if len(remessa) != 1 {
		t.Fatalf("esperava EXATAMENTE 1 chave de remessa depois de 3 ciclos, vieram %d: %v\n"+
			"Com mais de uma, o core-api passa a escolher desfecho pela ordem alfabética da chave — "+
			"e ele recusa a segunda mudança inclusive de falha para sucesso.", len(remessa), remessa)
	}
	if remessa[0] != "status/"+remittanceName+".json" {
		t.Errorf("a chave de remessa mudou de forma: %q", remessa[0])
	}

	// Houve reprocessamento, então tem de haver rastro dele — e sob chave PRÓPRIA, nunca na chave da
	// remessa.
	//
	// A contagem não é afirmada de propósito. O relógio deste harness é congelado, e duas tentativas
	// recusadas no MESMO segundo produzem a mesma chave de duplicado: a segunda sobrescreve a
	// primeira. Isso é limitação conhecida e aceita — o que o carimbo protege é o status ORIGINAL da
	// remessa (que é o que faria alguém reenviar um pagamento já feito), não a distinção entre dois
	// duplicados, que carregam a mesma informação.
	if len(duplicado) == 0 {
		t.Error("os reprocessamentos não deixaram rastro; sem eles ninguém sabe que houve tentativa recusada")
	}
	if len(recepcao) != 0 {
		t.Errorf("um ciclo de transmissão não publica status de recepção; veio %v", recepcao)
	}
	if len(outras) != 0 {
		t.Errorf("chave em status/ que o consumidor não sabe classificar: %v", outras)
	}
}
