package agent_test

// Envelope órfão (#13) — publicação que falha e o ciclo seguinte que a retoma.
//
// O que estes testes afirmam não é que a publicação funciona: é que uma publicação FALHA deixa
// rastro durável, que o ciclo seguinte a completa, e que o objeto no bucket deixa de ficar sem
// desfecho. O órfão é o defeito, e ele só é observável em dois ciclos.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/agent"
	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
	"github.com/ERP-Bem-Comum/van-agent/internal/ledger"
	"github.com/ERP-Bem-Comum/van-agent/internal/spool"
	"github.com/ERP-Bem-Comum/van-agent/internal/stcp/stcpfake"
)

// storeComPutQuebrado é o bucket em memória com o `Put` de `status/` sob controle do teste.
//
// Ele embrulha o duplo de produção em vez de o duplo ganhar um modo de falha: `bucket.Memory` vive
// em código de produção porque o ensaio o usa, e um gatilho de falha ali seria mecanismo de teste
// num caminho que roda contra instalação real.
type storeComPutQuebrado struct {
	*bucket.Memory
	// falharEmStatus liga a recusa de publicar. Só `status/` é afetado: o depósito do objeto
	// precisa continuar funcionando, senão o cenário testado seria outro (nada depositado, nada
	// órfão).
	falharEmStatus bool
	tentativas     int
}

func (s *storeComPutQuebrado) Put(ctx context.Context, key string, content []byte) error {
	if s.falharEmStatus && strings.HasPrefix(key, envelope.StatusPrefix) {
		s.tentativas++
		return errStatusIndisponivel
	}
	return s.Memory.Put(ctx, key, content)
}

var errStatusIndisponivel = &erroDeBucket{"prefixo de status indisponível (encenado pelo teste)"}

type erroDeBucket struct{ msg string }

func (e *erroDeBucket) Error() string { return e.msg }

// órfãoHarness monta a instalação com registro de pendências REAL em disco, como o resto da suíte:
// duplicar o registro tornaria a afirmação sobre durabilidade uma tautologia.
type orfaoHarness struct {
	t        *testing.T
	store    *storeComPutQuebrado
	pending  *ledger.FilePendingEnvelopes
	fake     *stcpfake.Fake
	ag       *agent.Agent
	prefixes bucket.Prefixes
	now      time.Time
}

// pendenciaComSaveQuebrado encena o disco local recusando a escrita do registro — disco cheio,
// permissão perdida, arquivo travado por antivírus na máquina Windows.
//
// Embrulha o registro REAL em vez de substituí-lo: o que interessa é o comportamento do agente
// quando o `Save` falha, não um registro inteiro de mentira.
type pendenciaComSaveQuebrado struct {
	ledger.PendingEnvelopes
	falhasRestantes int
	tentativas      int
}

func (p *pendenciaComSaveQuebrado) Save(key, fileName, body string, at time.Time) error {
	p.tentativas++
	if p.falhasRestantes > 0 {
		p.falhasRestantes--
		return errRegistroIndisponivel
	}
	return p.PendingEnvelopes.Save(key, fileName, body, at)
}

var errRegistroIndisponivel = &erroDeBucket{"registro de pendências indisponível (encenado pelo teste)"}

// newOrfaoHarness monta a instalação. O parâmetro variádico permite embrulhar o registro de
// pendências sem que os chamadores existentes mudem — e sem duplicar quarenta linhas de preparo.
func newOrfaoHarness(t *testing.T, envolver ...func(ledger.PendingEnvelopes) ledger.PendingEnvelopes) *orfaoHarness {
	t.Helper()

	root := t.TempDir()
	outboundDir := filepath.Join(root, "stcp", "SAIDA")
	backupDir := filepath.Join(root, "stcp", "BACKUP")
	inboundDir := filepath.Join(root, "stcp", "ENTRADA")
	receivedDir := filepath.Join(root, "stcp", "RECEBIDOS")
	logDir := filepath.Join(root, "stcp", "LOG")
	for _, d := range []string{outboundDir, backupDir, inboundDir, receivedDir, logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("preparar %s: %v", d, err)
		}
	}

	led, err := ledger.NewFileLedger(filepath.Join(root, "ledger"))
	if err != nil {
		t.Fatalf("registro: %v", err)
	}
	index, err := ledger.NewFileReceptionIndex(filepath.Join(root, "ledger", "recepcao"))
	if err != nil {
		t.Fatalf("índice de recepção: %v", err)
	}
	pending, err := ledger.NewFilePendingEnvelopes(filepath.Join(root, "ledger", "pendentes"))
	if err != nil {
		t.Fatalf("registro de pendências: %v", err)
	}

	sp, err := spool.NewDir(spool.Config{
		OutboundDir: outboundDir, BackupDir: backupDir, InboundDir: inboundDir,
		ReceivedDir: receivedDir, LogDir: logDir, TransferLogGlob: "*.LOG",
	})
	if err != nil {
		t.Fatalf("pastas do cliente: %v", err)
	}

	h := &orfaoHarness{
		t:        t,
		store:    &storeComPutQuebrado{Memory: bucket.NewMemory()},
		pending:  pending,
		fake:     stcpfake.New(outboundDir, backupDir, filepath.Join(logDir, "20260818.LOG")),
		prefixes: bucket.DefaultPrefixes(),
		now:      time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
	}
	h.fake.InboundDir = inboundDir
	h.fake.Now = func() time.Time { return h.now }

	var registro ledger.PendingEnvelopes = pending
	for _, e := range envolver {
		registro = e(registro)
	}

	ag, err := agent.New(h.store, led, h.fake, sp, agent.Config{
		Prefixes:    h.prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return h.now },
	}, agent.WithReceptionIndex(index), agent.WithPendingEnvelopes(registro))
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}
	h.ag = ag
	return h
}

func (h *orfaoHarness) pendentes() []ledger.PendingEnvelope {
	h.t.Helper()
	out, err := h.pending.List()
	if err != nil {
		h.t.Fatalf("listar pendências: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// CA1/CA2 — recepção: a publicação falha, o ciclo seguinte a completa
// ─────────────────────────────────────────────────────────────────────────────

func TestCA13_RecepcaoComPublicacaoQueFalhaDeixaPendenciaERepublicaNoCicloSeguinte(t *testing.T) {
	h := newOrfaoHarness(t)
	h.store.falharEmStatus = true
	h.fake.Incoming = append(h.fake.Incoming, stcpfake.Incoming{
		Name: returnName, Content: []byte(returnContent), Logged: true,
	})

	// Ciclo 1: o objeto entra em retorno/, mas o envelope não sai.
	sum, err := h.ag.ReceiveCycle(context.Background())
	if err != nil {
		t.Fatalf("o ciclo não pode abortar por falha de publicação: %v", err)
	}
	if len(sum.Errs()) == 0 {
		t.Error("a falha de publicação precisa aparecer nos erros do ciclo; em silêncio ninguém vê o órfão")
	}
	if _, err := h.store.Get(context.Background(), h.prefixes.Returns+returnName); err != nil {
		t.Fatalf("o objeto precisava ter sido depositado antes da publicação falhar: %v", err)
	}
	statusKey := sum.Outcomes[0].StatusKey
	if _, err := h.store.Get(context.Background(), statusKey); err == nil {
		t.Fatal("o envelope não deveria existir: a publicação falhou")
	}

	// CA1 — a pendência ficou registrada, e sabe a que arquivo se refere.
	pend := h.pendentes()
	if len(pend) != 1 {
		t.Fatalf("esperava 1 pendência registrada, veio %d", len(pend))
	}
	if pend[0].Chave != statusKey {
		t.Errorf("pendência aponta %q, esperava %q", pend[0].Chave, statusKey)
	}
	if pend[0].Arquivo != returnName {
		t.Errorf("pendência precisa nomear o arquivo de origem; veio %q", pend[0].Arquivo)
	}

	// Ciclo 2, com o bucket de volta: a reconciliação publica o que ficou para trás.
	h.store.falharEmStatus = false
	h.now = h.now.Add(5 * time.Minute)
	sum2, err := h.ag.ReceiveCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo de recepção abortou: %v", err)
	}
	if sum2.Republicados != 1 {
		t.Errorf("esperava 1 envelope republicado, veio %d", sum2.Republicados)
	}
	if errs := sum2.Errs(); len(errs) > 0 {
		t.Errorf("o segundo ciclo não deveria acumular erros: %v", errs)
	}

	// CA2 — o envelope publicado é o ORIGINAL: o desfecho não mudou, só a publicação falhara.
	raw, err := h.store.Get(context.Background(), statusKey)
	if err != nil {
		t.Fatalf("o envelope precisava existir depois da reconciliação: %v", err)
	}
	if string(raw) != pend[0].Corpo {
		t.Error("o envelope republicado difere do original; reconstruí-lo afirmaria outro desfecho")
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope republicado ilegível: %v", err)
	}
	if env.Situacao != envelope.Reception || env.Arquivo != returnName {
		t.Errorf("o envelope republicado descreve outra coisa: situacao=%q arquivo=%q", env.Situacao, env.Arquivo)
	}

	// CA3 — a pendência sai depois do sucesso, senão vira republicação eterna.
	if p := h.pendentes(); len(p) != 0 {
		t.Errorf("a pendência precisava ser limpa após a publicação; restaram %d", len(p))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// O último caminho que ainda produzia órfão: disco local E bucket falhando juntos
//
// Levantado por uma pergunta do core-api — "existe algum estado em que a pendência é abandonada?".
// Existia: se o `Save` falhasse e o `Put` falhasse na mesma passagem, o desfecho não era publicado
// e não ficava registrado; o passo seguinte movia o objeto para fora da fila assim mesmo e o
// registro já dizia `done`. Nada voltava a passar por ali.
//
// Do lado de quem consome, esse objeto vira quarentena por proveniência ausente — indistinguível de
// um arquivo que nunca deveria ter entrado. Um pagamento nosso parecendo arquivo alheio, para sempre.
// ─────────────────────────────────────────────────────────────────────────────

func TestCA13_RegistroQueFalhaUmaVezEhTentadoDeNovoEAPendenciaSobrevive(t *testing.T) {
	// Falha SÓ na primeira chamada: é a causa transitória — disco momentaneamente cheio, arquivo
	// travado por um instante. A segunda tentativa é o que separa "perdeu o desfecho" de
	// "republicou no ciclo seguinte".
	var registro *pendenciaComSaveQuebrado
	h := newOrfaoHarness(t, func(p ledger.PendingEnvelopes) ledger.PendingEnvelopes {
		registro = &pendenciaComSaveQuebrado{PendingEnvelopes: p, falhasRestantes: 1}
		return registro
	})
	h.store.falharEmStatus = true
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))

	sum, err := h.ag.TransmitCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo abortou: %v", err)
	}
	if registro.tentativas < 2 {
		t.Fatalf("o registro precisava ser tentado de novo depois de o `Put` falhar; tentativas: %d",
			registro.tentativas)
	}
	if len(h.pendentes()) != 1 {
		t.Fatalf("a pendência precisa existir depois da segunda tentativa; veio %d", len(h.pendentes()))
	}
	for _, e := range sum.Errs() {
		if errors.Is(e, agent.ErrOrphanEnvelope) {
			t.Errorf("não é órfão: a retomada existe. Erro: %v", e)
		}
	}

	// E a prova de que a retomada é real: o ciclo seguinte, com o bucket de volta, publica.
	h.store.falharEmStatus = false
	if _, err := h.ag.TransmitCycle(context.Background()); err != nil {
		t.Fatalf("segundo ciclo abortou: %v", err)
	}
	if len(h.pendentes()) != 0 {
		t.Errorf("a pendência deveria ter sido limpa após republicar; restam %d", len(h.pendentes()))
	}
	if _, err := h.store.Get(context.Background(), sum.Outcomes[0].StatusKey); err != nil {
		t.Errorf("o envelope deveria ter sido republicado: %v", err)
	}
}

func TestCA13_DiscoEBucketFalhandoJuntosAcusamOrfaoEmVezDeSilenciar(t *testing.T) {
	// O disco NUNCA aceita: nem a primeira tentativa nem a segunda. Aqui o código chegou ao fim do
	// que pode fazer — e o que ainda está na mão dele é não deixar isto parecer falha comum.
	var registro *pendenciaComSaveQuebrado
	h := newOrfaoHarness(t, func(p ledger.PendingEnvelopes) ledger.PendingEnvelopes {
		registro = &pendenciaComSaveQuebrado{PendingEnvelopes: p, falhasRestantes: 99}
		return registro
	})
	h.store.falharEmStatus = true
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))

	sum, err := h.ag.TransmitCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo abortou: %v", err)
	}

	// A pendência de fato não existe — o cenário é o pior caso, não uma encenação parcial.
	if n := len(h.pendentes()); n != 0 {
		t.Fatalf("cenário inválido: a pendência não podia existir; vieram %d", n)
	}
	if registro.tentativas < 2 {
		t.Errorf("o registro precisa ser tentado duas vezes antes de desistir; tentativas: %d",
			registro.tentativas)
	}

	// O que se exige: que o erro seja RECONHECÍVEL como órfão, e não só mais uma linha na lista.
	// Uma falha comum de publicação diz "o próximo ciclo resolve"; esta diz "vá olhar o bucket".
	// Saírem pela mesma porta é o que faz alguém tratar a segunda como a primeira.
	var achou bool
	for _, e := range sum.Errs() {
		if errors.Is(e, agent.ErrOrphanEnvelope) {
			achou = true
		}
	}
	if !achou {
		t.Errorf("o ciclo precisa acusar %v; erros vieram como: %v",
			agent.ErrOrphanEnvelope, errors.Join(sum.Errs()...))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA5 — o mesmo defeito existe na transmissão, e o mesmo mecanismo o cobre
// ─────────────────────────────────────────────────────────────────────────────

func TestCA13_TransmissaoComPublicacaoQueFalhaEhRetomadaNoCicloSeguinte(t *testing.T) {
	h := newOrfaoHarness(t)
	h.store.falharEmStatus = true
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))

	sum, err := h.ag.TransmitCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo de transmissão abortou: %v", err)
	}
	if len(sum.Errs()) == 0 {
		t.Error("a falha de publicação precisa aparecer nos erros do ciclo")
	}
	statusKey := sum.Outcomes[0].StatusKey
	if _, err := h.store.Get(context.Background(), statusKey); err == nil {
		t.Fatal("o envelope não deveria existir: a publicação falhou")
	}
	// O objeto saiu da fila mesmo assim — é o que torna o órfão permanente sem a retomada.
	if sum.Outcomes[0].MovedTo == "" {
		t.Fatal("cenário inválido: o objeto precisa ter saído da fila para o órfão existir")
	}
	if len(h.pendentes()) != 1 {
		t.Fatalf("a transmissão precisa registrar a pendência igual à recepção; veio %d", len(h.pendentes()))
	}

	h.store.falharEmStatus = false
	h.now = h.now.Add(5 * time.Minute)
	sum2, err := h.ag.TransmitCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo de transmissão abortou: %v", err)
	}
	if sum2.Republicados != 1 {
		t.Errorf("esperava 1 envelope republicado, veio %d", sum2.Republicados)
	}
	if _, err := h.store.Get(context.Background(), statusKey); err != nil {
		t.Errorf("a remessa transmitida continuou sem desfecho publicado: %v", err)
	}
	if p := h.pendentes(); len(p) != 0 {
		t.Errorf("pendência não foi limpa; restaram %d", len(p))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA6 — enquanto não publica, permanece; e o ciclo continua reclamando
// ─────────────────────────────────────────────────────────────────────────────

func TestCA13_PendenciaQueContinuaFalhandoPermaneceEContinuaAcusandoErro(t *testing.T) {
	h := newOrfaoHarness(t)
	h.store.falharEmStatus = true
	h.fake.Incoming = append(h.fake.Incoming, stcpfake.Incoming{
		Name: returnName, Content: []byte(returnContent), Logged: true,
	})

	if _, err := h.ag.ReceiveCycle(context.Background()); err != nil {
		t.Fatalf("ciclo 1: %v", err)
	}
	if len(h.pendentes()) != 1 {
		t.Fatalf("esperava a pendência registrada")
	}

	// Segundo ciclo, bucket ainda fora: a pendência não pode ser descartada.
	h.now = h.now.Add(5 * time.Minute)
	sum2, err := h.ag.ReceiveCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo 2: %v", err)
	}
	if sum2.Republicados != 0 {
		t.Errorf("nada podia ter sido republicado com o bucket fora; veio %d", sum2.Republicados)
	}
	if sum2.ReconcileErr == nil {
		t.Error("a republicação que falhou precisa acusar erro; desistir em silêncio recria o órfão")
	}
	if len(sum2.Errs()) == 0 {
		t.Error("o erro de reconciliação precisa fazer o ciclo sair com falha")
	}
	if len(h.pendentes()) != 1 {
		t.Errorf("a pendência precisa permanecer enquanto não publicar; veio %d", len(h.pendentes()))
	}

	// E quando o bucket volta, ela sai — sem intervenção nenhuma.
	h.store.falharEmStatus = false
	h.now = h.now.Add(5 * time.Minute)
	sum3, err := h.ag.ReceiveCycle(context.Background())
	if err != nil {
		t.Fatalf("ciclo 3: %v", err)
	}
	if sum3.Republicados != 1 {
		t.Errorf("a pendência devia ter saído quando o bucket voltou; republicados=%d", sum3.Republicados)
	}
	if len(h.pendentes()) != 0 {
		t.Error("a pendência precisava ter sido limpa")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA4 — o custo da reconciliação é proporcional às PENDÊNCIAS, não ao histórico
// ─────────────────────────────────────────────────────────────────────────────

// O índice de recepção cresce para sempre — um registro por conteúdo já recebido — e o ciclo roda a
// cada 5 minutos. Se a reconciliação varresse o índice, o custo cresceria sem teto. Este teste
// prende o mecanismo ao diretório de pendências, que normalmente está vazio.
func TestCA13_ReconciliacaoNaoVarreOHistoricoDeRecepcoes(t *testing.T) {
	h := newOrfaoHarness(t)

	// Vários ciclos bem-sucedidos deixam histórico no índice de recepção...
	for i, nome := range []string{
		"PAG_000000.20260818110000_0001.RET",
		"PAG_000000.20260818110000_0002.RET",
		"PAG_000000.20260818110000_0003.RET",
	} {
		h.fake.Incoming = append(h.fake.Incoming, stcpfake.Incoming{
			Name: nome, Content: []byte(returnContent + string(rune('A'+i))), Logged: true,
		})
		h.now = h.now.Add(time.Minute)
		if _, err := h.ag.ReceiveCycle(context.Background()); err != nil {
			t.Fatalf("ciclo %d: %v", i, err)
		}
	}

	// ...e nenhuma pendência, porque nada falhou.
	if p := h.pendentes(); len(p) != 0 {
		t.Fatalf("ciclos bem-sucedidos não podem deixar pendência; vieram %d", len(p))
	}

	h.now = h.now.Add(time.Minute)
	n, err := h.ag.ReconcilePending(context.Background())
	if err != nil {
		t.Fatalf("reconciliação: %v", err)
	}
	if n != 0 {
		t.Errorf("sem pendências, nada há para republicar; veio %d", n)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// A durabilidade do registro, no nível do próprio componente
// ─────────────────────────────────────────────────────────────────────────────

// Um registro que só existisse em memória perderia a pendência exatamente no caso que ela existe
// para cobrir: o processo morre. Aqui um registro NOVO, apontando para o mesmo diretório, enxerga o
// que o anterior gravou — que é o que acontece na passada seguinte do agendador.
func TestCA13_PendenciaSobreviveAoProcesso(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pendentes")

	primeiro, err := ledger.NewFilePendingEnvelopes(dir)
	if err != nil {
		t.Fatalf("registro: %v", err)
	}
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if err := primeiro.Save("status/X.json", "X.REM", `{"arquivo":"X.REM"}`, at); err != nil {
		t.Fatalf("gravar pendência: %v", err)
	}

	segundo, err := ledger.NewFilePendingEnvelopes(dir)
	if err != nil {
		t.Fatalf("segundo registro: %v", err)
	}
	got, err := segundo.List()
	if err != nil {
		t.Fatalf("listar: %v", err)
	}
	if len(got) != 1 || got[0].Chave != "status/X.json" || got[0].Corpo != `{"arquivo":"X.REM"}` {
		t.Fatalf("a pendência não sobreviveu ao processo: %+v", got)
	}

	if err := segundo.Clear("status/X.json"); err != nil {
		t.Fatalf("limpar: %v", err)
	}
	if got, _ := segundo.List(); len(got) != 0 {
		t.Errorf("a limpeza não removeu a pendência: %+v", got)
	}
	// Limpar o que não existe é o caso comum (toda publicação bem-sucedida chama), e não é erro.
	if err := segundo.Clear("status/nunca-existiu.json"); err != nil {
		t.Errorf("limpar pendência ausente não pode falhar: %v", err)
	}
}
