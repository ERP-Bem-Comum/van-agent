package agent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// O nome do arquivo de RETORNO segue a convenção do BANCO, não a nossa: quem o atribui é ele. É por
// isso que o agente nunca deduplica por nome.
const returnName = "PAG_000000.20260818110000_0001.RET"

const returnContent = "0372345...CONTEUDO CNAB DE RETORNO FICTICIO..."

func sha256Of(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// receiveHarness monta a instalação simulada do lado da recepção: pastas reais em disco, índice
// real, bucket em memória e o duplo do cliente encenando o banco entregando arquivos.
type receiveHarness struct {
	t          *testing.T
	store      *bucket.Memory
	index      *ledger.FileReceptionIndex
	fake       *stcpfake.Fake
	ag         *agent.Agent
	prefixes   bucket.Prefixes
	inboundDir string
	receivedin string
	now        time.Time
}

func newReceiveHarness(t *testing.T) *receiveHarness {
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
	// Diretório PRÓPRIO: os dois índices não podem colidir (um indexa nome de remessa, o outro hash
	// de conteúdo).
	index, err := ledger.NewFileReceptionIndex(filepath.Join(root, "ledger", "recepcao"))
	if err != nil {
		t.Fatalf("índice de recepção: %v", err)
	}

	sp, err := spool.NewDir(spool.Config{
		OutboundDir:     outboundDir,
		BackupDir:       backupDir,
		InboundDir:      inboundDir,
		ReceivedDir:     receivedDir,
		LogDir:          logDir,
		TransferLogGlob: "*.LOG",
	})
	if err != nil {
		t.Fatalf("pastas do cliente: %v", err)
	}

	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	fake := stcpfake.New(outboundDir, backupDir, filepath.Join(logDir, "20260818.LOG"))
	fake.InboundDir = inboundDir

	store := bucket.NewMemory()
	prefixes := bucket.DefaultPrefixes()

	h := &receiveHarness{
		t: t, store: store, index: index, fake: fake, prefixes: prefixes,
		inboundDir: inboundDir, receivedin: receivedDir, now: now,
	}

	// O relógio lê do harness a cada chamada: os testes de duplicidade precisam encenar ciclos em
	// momentos DIFERENTES, e um relógio congelado na construção faria duas recepções distintas
	// caírem na mesma chave — escondendo justamente o que elas verificam.
	ag, err := agent.New(store, led, fake, sp, agent.Config{
		Prefixes:    prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return h.now },
	}, agent.WithReceptionIndex(index))
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}
	h.ag = ag
	h.avancaRelogio()
	return h
}

// entrega enfileira um arquivo para o próximo acionamento em modo de recepção.
func (h *receiveHarness) entrega(name, content string, logged bool) {
	h.t.Helper()
	h.fake.Incoming = append(h.fake.Incoming, stcpfake.Incoming{
		Name: name, Content: []byte(content), Logged: logged,
	})
}

// avancaRelogio sincroniza o duplo do cliente com o relógio do harness.
func (h *receiveHarness) avancaRelogio() {
	h.fake.Now = func() time.Time { return h.now }
}

func (h *receiveHarness) hasKey(key string) bool {
	_, err := h.store.Get(context.Background(), key)
	return err == nil
}

func (h *receiveHarness) run() agent.ReceiveSummary {
	h.t.Helper()
	sum, err := h.ag.ReceiveCycle(context.Background())
	if err != nil {
		h.t.Fatalf("ciclo de recepção abortou: %v", err)
	}
	return sum
}

// desfechoDe acha o desfecho de um arquivo pelo nome. Um ciclo com mais de um arquivo não garante
// ordem estável de interesse ao teste, e indexar por posição faria a asserção depender dela.
func (h *receiveHarness) desfechoDe(sum agent.ReceiveSummary, name string) agent.ReceiveOutcome {
	h.t.Helper()
	for _, o := range sum.Outcomes {
		if o.FileName == name {
			return o
		}
	}
	h.t.Fatalf("nenhum desfecho para %q no ciclo", name)
	return agent.ReceiveOutcome{}
}

func (h *receiveHarness) envelopeEm(key string) envelope.Envelope {
	h.t.Helper()
	raw, err := h.store.Get(context.Background(), key)
	if err != nil {
		h.t.Fatalf("envelope ausente em %q; chaves: %v", key, h.store.Keys())
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		h.t.Fatalf("envelope em %q ilegível: %v", key, err)
	}
	return env
}

func (h *receiveHarness) objeto(key string) []byte {
	h.t.Helper()
	raw, err := h.store.Get(context.Background(), key)
	if err != nil {
		h.t.Fatalf("objeto ausente em %q; chaves: %v", key, h.store.Keys())
	}
	return raw
}

// ─────────────────────────────────────────────────────────────────────────────
// CA1 — o arquivo chega ao prefixo de retorno íntegro e NÃO interpretado
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_ArquivoRecebidoVaiParaORetornoComConteudoIntegro(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)

	sum := h.run()

	if !sum.ClientInvoked {
		t.Fatal("o ciclo de recepção precisa acionar o cliente; sem isso é varredura de pasta")
	}
	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}

	got := sum.Outcomes[0]
	if got.Situation != envelope.Reception {
		t.Errorf("situação = %q, esperava %q", got.Situation, envelope.Reception)
	}

	// Byte a byte: o agente nunca abre CNAB, e um único byte alterado é um arquivo que o core-api
	// recusa sem que ninguém saiba por quê.
	depositado := h.objeto(h.prefixes.Returns + returnName)
	if string(depositado) != returnContent {
		t.Errorf("o conteúdo mudou no caminho:\n  entregue:  %q\n  depositado: %q", returnContent, depositado)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA2 — o envelope carrega nome, hash, linhas cruas e carimbo
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_EnvelopeCarregaProveniencia(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)

	sum := h.run()

	env := h.envelopeEm(sum.Outcomes[0].StatusKey)
	if env.Arquivo != returnName {
		t.Errorf("arquivo = %q, esperava %q", env.Arquivo, returnName)
	}
	if env.ExecutadoEm == "" {
		t.Error("o envelope precisa do carimbo de tempo")
	}
	if env.Recepcao == nil {
		t.Fatal("o envelope de recepção precisa carregar a proveniência")
	}
	if env.Recepcao.Sha256 != sha256Of(returnContent) {
		t.Errorf("sha256 = %q, esperava %q", env.Recepcao.Sha256, sha256Of(returnContent))
	}
	if env.Recepcao.Chave != h.prefixes.Returns+returnName {
		t.Errorf("chave = %q, esperava %q", env.Recepcao.Chave, h.prefixes.Returns+returnName)
	}
	if !env.Recepcao.Correlacionado {
		t.Error("o arquivo tem linha no log deste ciclo; deveria constar como correlacionado")
	}
	// As linhas vão CRUAS, e precisam ser as de RECEPÇÃO daquele arquivo.
	if len(env.LogTransferencia) == 0 {
		t.Fatal("o envelope deveria carregar as linhas do log que correlacionam o arquivo")
	}
	for _, l := range env.LogTransferencia {
		if !strings.Contains(l, returnName) {
			t.Errorf("linha de log não corresponde ao arquivo: %q", l)
		}
	}
}

// A correlação é por linha de RECEPÇÃO, não por nome solto no log. Uma linha de transmissão do
// mesmo nome correlacionaria o arquivo errado — um retorno lido como remessa.
func TestCA5_CorrelacaoIgnoraLinhasDeTransmissao(t *testing.T) {
	h := newReceiveHarness(t)

	// Um ciclo de transmissão deixa linhas 0004/0005 para este nome...
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))
	if _, err := h.ag.TransmitCycle(context.Background()); err != nil {
		t.Fatalf("ciclo de transmissão: %v", err)
	}
	// ...e agora o mesmo NOME aparece na pasta de entrada, sem linha de recepção.
	h.entrega(remittanceName, "conteúdo diferente, sem linha de recepção", false)

	sum := h.run()

	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}
	if sum.Outcomes[0].Correlated {
		t.Error("linha de TRANSMISSÃO não pode correlacionar um arquivo recebido")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA3 — sem correlação, deposita ASSIM MESMO, declarando a ausência
// ─────────────────────────────────────────────────────────────────────────────

// O log DESTE ciclo foi lido e não trazia a linha do arquivo. É o caso genuinamente suspeito, e o
// único em que a ausência de correlação diz algo sobre o ARQUIVO.
//
// Para o log existir com linhas desta execução, outro arquivo chega correlacionado no mesmo ciclo —
// que é como a instalação real se comporta: o log é do ciclo, não do arquivo.
func TestCA5_LogLidoSemALinhaDoArquivoDeclaraAusenciaDeCorrelacao(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega("PAG_000000.20260818110000_0002.RET", "outro retorno, este correlacionado", true)
	h.entrega(returnName, returnContent, false) // entregue SEM deixar linha no log

	sum := h.run()

	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if !sum.LogDoCicloLido {
		t.Fatal("o log deste ciclo tem linhas e foi lido; o resumo precisa afirmá-lo")
	}
	// Erra-se para MAIS: descartar em silêncio um arquivo do banco é o desfecho que ninguém percebe.
	if _, err := h.store.Get(context.Background(), h.prefixes.Returns+returnName); err != nil {
		t.Fatalf("o arquivo sem correlação precisava ser depositado assim mesmo: %v", err)
	}

	env := h.envelopeEm(h.desfechoDe(sum, returnName).StatusKey)
	if env.Recepcao == nil || env.Recepcao.Correlacionado {
		t.Error("o envelope precisa declarar que a origem não foi correlacionada")
	}
	if !env.Recepcao.LogDoCicloLido {
		t.Error("o log foi lido; sem isso o consumidor não distingue \"não sei\" de \"sei que não tinha\"")
	}
	if !strings.Contains(env.Detalhe, "SEM linha correspondente") {
		t.Errorf("o detalhe precisa dizer que não houve correlação; veio: %q", env.Detalhe)
	}
	// E o hash continua lá: é ele que permite ao consumidor decidir sem reabrir o objeto.
	if env.Recepcao.Sha256 != sha256Of(returnContent) {
		t.Errorf("sha256 = %q, esperava %q", env.Recepcao.Sha256, sha256Of(returnContent))
	}
}

// Sem log nenhum, a não-correlação não é sinal sobre o arquivo — é sobre a instalação.
//
// Este é o caso que, tratado como o de cima, faria o consumidor represar pagamento confirmado por
// causa de um padrão de log mal configurado. O envelope precisa dizer que o agente NÃO SABE.
func TestCA5_SemLogDoCicloAAusenciaDeCorrelacaoNaoAfirmaNadaSobreOArquivo(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, false) // nada é escrito no log

	sum := h.run()

	if errs := sum.Errs(); len(errs) > 0 {
		t.Fatalf("o ciclo acumulou erros: %v", errs)
	}
	if sum.LogDoCicloLido {
		t.Fatal("não havia log deste ciclo; afirmar que foi lido é o erro que o campo existe para impedir")
	}
	// O arquivo entra do mesmo jeito: a dúvida é sobre a evidência, não sobre o conteúdo.
	if _, err := h.store.Get(context.Background(), h.prefixes.Returns+returnName); err != nil {
		t.Fatalf("o arquivo precisava ser depositado assim mesmo: %v", err)
	}

	env := h.envelopeEm(sum.Outcomes[0].StatusKey)
	if env.Recepcao == nil || env.Recepcao.Correlacionado {
		t.Error("sem log não há como correlacionar")
	}
	if env.Recepcao.LogDoCicloLido {
		t.Error("o envelope está afirmando ter lido um log que não existe")
	}
	if !strings.Contains(env.Detalhe, "configuração do log") {
		t.Errorf("o detalhe precisa apontar a INSTALAÇÃO, não o arquivo; veio: %q", env.Detalhe)
	}
}

// O modo de falha diário: o padrão casa o log de ONTEM, a leitura dá certo, e nenhuma linha é desta
// execução.
//
// Sem a âncora temporal, o agente leria um log real, concluiria "li o log e não havia a linha" e
// publicaria não-correlação com aparência de certeza — para TODOS os retornos do primeiro ciclo do
// dia, todo dia. É o caso que justifica o campo existir.
func TestCA5_LogDeOutroCicloNaoContaComoLogDesteCiclo(t *testing.T) {
	h := newReceiveHarness(t)

	// Um ciclo ontem deixou o log dele preenchido...
	h.now = h.now.AddDate(0, 0, -1)
	h.avancaRelogio()
	h.entrega("PAG_000000.20260817110000_0001.RET", "retorno de ontem", true)
	h.run()

	// ...e hoje o cliente ainda não escreveu log novo, mas o padrão casa o de ontem.
	h.now = h.now.AddDate(0, 0, 1)
	h.avancaRelogio()
	h.entrega(returnName, returnContent, false)

	sum := h.run()

	if sum.LogDoCicloLido {
		t.Fatal("o log casado é de outro ciclo; tratá-lo como deste é o furo diário que a janela fecha")
	}
	env := h.envelopeEm(sum.Outcomes[0].StatusKey)
	if env.Recepcao.LogDoCicloLido {
		t.Error("o envelope afirma ter lido o log desta execução, e leu o de ontem")
	}
	if !strings.Contains(env.Detalhe, "configuração do log") {
		t.Errorf("o detalhe precisa apontar a instalação; veio: %q", env.Detalhe)
	}
}

// Linha de recepção de um ciclo ANTERIOR, com o mesmo nome, não pode correlacionar o arquivo de
// agora.
//
// Bug que existia antes do campo novo: a correlação filtrava por nome e operação, nunca por tempo.
// O cenário é real — o banco reenvia o mesmo nome com conteúdo corrigido; o hash acerta que é
// arquivo novo, mas a linha velha do log do dia dava uma correlação que ninguém observou acontecer.
func TestCA5_LinhaDeRecepcaoAntigaComMesmoNomeNaoCorrelaciona(t *testing.T) {
	h := newReceiveHarness(t)

	// Ciclo 1: o arquivo chega e é registrado no log.
	h.entrega(returnName, returnContent, true)
	h.run()

	// Ciclo 2, mais tarde: o MESMO nome volta com conteúdo diferente, e desta vez sem linha nova.
	h.now = h.now.Add(2 * time.Hour)
	h.avancaRelogio()
	h.entrega(returnName, "conteúdo corrigido pelo banco, mesmo nome", false)

	sum := h.run()

	if len(sum.Outcomes) != 1 {
		t.Fatalf("esperava 1 desfecho, veio %d", len(sum.Outcomes))
	}
	if sum.Outcomes[0].Correlated {
		t.Error("a linha correlacionada é do ciclo anterior; ela não prova a origem do arquivo de agora")
	}
}

// O log diz que recebeu um arquivo que não está na pasta. O agente NÃO inventa arquivo a partir do
// log — mas o caso precisa aparecer, porque um arquivo que sumiu antes de alguém olhar é o desfecho
// que ninguém percebe.
func TestCA5_ArquivoNoLogEAusenteNaPastaApareceNoResumo(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)
	h.run() // primeiro ciclo: recebe e arquiva, deixando as linhas no log

	sum := h.run() // segundo ciclo: o log ainda tem as linhas, mas a pasta está vazia

	if len(sum.Outcomes) != 0 {
		t.Errorf("nada devia ser processado no segundo ciclo; veio %d desfecho(s)", len(sum.Outcomes))
	}
	if len(sum.LoggedButAbsent) != 1 || sum.LoggedButAbsent[0] != returnName {
		t.Errorf("o arquivo do log ausente da pasta deveria aparecer no resumo; veio %v", sum.LoggedButAbsent)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA5 — ciclo interrompido vira revisão explícita, nunca sucesso presumido
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_CicloInterrompidoViraRevisaoENaoSucessoPresumido(t *testing.T) {
	h := newReceiveHarness(t)

	// Encena a morte no meio: a intenção foi registrada e a conclusão nunca chegou. É o estado que
	// o disco guardaria se o processo tivesse sido morto entre registrar e depositar.
	if err := h.index.RecordIntent(sha256Of(returnContent), returnName, h.now); err != nil {
		t.Fatalf("registrar intenção: %v", err)
	}
	h.entrega(returnName, returnContent, true)

	sum := h.run()

	if sum.Outcomes[0].Situation != envelope.Review {
		t.Errorf("situação = %q, esperava %q — interrompido não vira sucesso presumido",
			sum.Outcomes[0].Situation, envelope.Review)
	}
	// E o arquivo é depositado do mesmo jeito: nunca descartar é mais forte que o alarme.
	if _, err := h.store.Get(context.Background(), h.prefixes.Returns+returnName); err != nil {
		t.Errorf("o arquivo precisava ser depositado mesmo com o desfecho em revisão: %v", err)
	}
	env := h.envelopeEm(sum.Outcomes[0].StatusKey)
	if !strings.Contains(env.Detalhe, "conferência humana") {
		t.Errorf("o detalhe deveria encaminhar para conferência humana; veio: %q", env.Detalhe)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA6 — nome que quebra fronteira: recusa ANTES de qualquer escrita no bucket
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_NomeQueQuebraFronteiraNaoEscreveNadaNoBucket(t *testing.T) {
	casos := []string{
		"..%2F..%2Fetc%2Fpasswd", // travessia depois de decodificar
		"PAG_1.duplicado-X.RET",  // viraria status de tentativa recusada aos olhos do consumidor
		"recepcao-2026.RET",      // viraria status de recepção
	}
	for _, nome := range casos {
		t.Run(nome, func(t *testing.T) {
			h := newReceiveHarness(t)
			h.entrega(nome, returnContent, true)

			sum := h.run()

			if len(sum.Errs()) == 0 {
				t.Error("esperava erro registrado para nome que quebra a fronteira")
			}
			// NADA no bucket: nem objeto, nem envelope. A chave do envelope deriva do nome, e
			// publicar com um nome sanitizado escreveria um desfecho sob uma chave que não
			// corresponde a arquivo nenhum.
			if chaves := h.store.Keys(); len(chaves) != 0 {
				t.Errorf("nada podia ter sido escrito no bucket; veio %v", chaves)
			}
			// E o arquivo continua na pasta de entrada, visível para quem for conferir.
			if _, err := os.Stat(filepath.Join(h.inboundDir, nome)); err != nil {
				t.Errorf("o arquivo deveria continuar na pasta de entrada: %v", err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// O arquivo sai da pasta de entrada DEPOIS de estar no bucket, nunca antes
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_ArquivoSoSaiDaEntradaDepoisDeEstarNoBucket(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)

	sum := h.run()

	if !sum.Outcomes[0].Archived {
		t.Error("o arquivo deveria ter saído da pasta de entrada depois de depositado")
	}
	if _, err := os.Stat(filepath.Join(h.inboundDir, returnName)); err == nil {
		t.Error("o arquivo continua na pasta de entrada; a próxima passada o reprocessaria")
	}
	if _, err := os.Stat(filepath.Join(h.receivedin, returnName)); err != nil {
		t.Errorf("o arquivo deveria estar na pasta de arquivados: %v", err)
	}
}

// Sem índice configurado o ciclo RECUSA rodar, em vez de operar sem memória do que já chegou.
func TestCA5_CicloDeRecepcaoRecusaRodarSemIndice(t *testing.T) {
	h := newReceiveHarness(t)
	semIndice, err := agent.New(h.store, nil, h.fake, nil, agent.Config{
		Prefixes:    h.prefixes,
		NamePattern: namePattern,
		Clock:       func() time.Time { return h.now },
	})
	if err != nil {
		t.Fatalf("montar agente: %v", err)
	}

	if _, err := semIndice.ReceiveCycle(context.Background()); err == nil {
		t.Error("o ciclo deveria recusar rodar sem índice de recepção")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Idempotência da recepção — as três combinações que o hash separa e o nome não
//
// A idempotência que protege o NEGÓCIO é a do efeito, e ela vive no core-api (chave de negócio:
// NSA + "Seu Número"). O que estes testes cobram é outra coisa, e é o que o agente pode garantir:
// que ele não PERDE e não CONFUNDE arquivos. Ele nunca abre CNAB e não conhece chave de negócio.
// ─────────────────────────────────────────────────────────────────────────────

// CA1 — mesmo nome, mesmo conteúdo: o objeto original não é sobrescrito.
func TestCA5_MesmoConteudoReaparecendoNaoSobrescreveEDeclaraDuplicidade(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)
	primeiro := h.run()

	chaveOriginal := primeiro.Outcomes[0].StoredKey
	envelopeOriginal := primeiro.Outcomes[0].StatusKey
	conteudoOriginal := h.objeto(chaveOriginal)

	// O banco reenvia o mesmo arquivo num ciclo posterior.
	h.now = h.now.Add(time.Hour)
	h.avancaRelogio()
	h.entrega(returnName, returnContent, true)
	segundo := h.run()

	if !segundo.Outcomes[0].Duplicate {
		t.Error("a reaparição do mesmo conteúdo deveria ser reconhecida como duplicada")
	}
	// O objeto original continua intacto. Sobrescrever um arquivo de retorno destrói evidência de um
	// pagamento — o pior lugar possível para perder registro.
	if string(h.objeto(chaveOriginal)) != string(conteudoOriginal) {
		t.Error("o objeto original foi sobrescrito")
	}
	// E o envelope da primeira recepção continua legível, sob a chave dele.
	if _, err := h.store.Get(context.Background(), envelopeOriginal); err != nil {
		t.Errorf("o envelope da recepção original sumiu: %v", err)
	}

	// O envelope do duplicado vem sob chave DISTINTA e aponta para a recepção anterior.
	dup := h.envelopeEm(segundo.Outcomes[0].StatusKey)
	if segundo.Outcomes[0].StatusKey == envelopeOriginal {
		t.Fatal("o envelope do duplicado sobrescreveu o da recepção original")
	}
	if dup.Recepcao == nil || !dup.Recepcao.Duplicado {
		t.Fatal("o envelope precisa declarar a recepção duplicada")
	}
	if dup.Recepcao.DuplicadoDe != chaveOriginal {
		t.Errorf("duplicadoDe = %q, esperava %q", dup.Recepcao.DuplicadoDe, chaveOriginal)
	}
}

// CA2 — mesmo NOME, conteúdo DIFERENTE: é arquivo novo, e os dois continuam recuperáveis.
//
// O nome é atribuído pelo banco e não é identificador. Tratá-lo como tal descartaria um retorno
// legítimo — a pior das duas falhas opostas que deduplicar por nome produz.
func TestCA5_MesmoNomeComConteudoDiferenteEhArquivoNovo(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)
	primeiro := h.run()
	chaveOriginal := primeiro.Outcomes[0].StoredKey

	const outroConteudo = "0372345...OUTRO LOTE, MESMO NOME..."
	h.now = h.now.Add(time.Hour)
	h.avancaRelogio()
	h.entrega(returnName, outroConteudo, true)
	segundo := h.run()

	if segundo.Outcomes[0].Duplicate {
		t.Error("conteúdo diferente NUNCA é duplicado, mesmo com o nome igual")
	}
	novaChave := segundo.Outcomes[0].StoredKey
	if novaChave == chaveOriginal {
		t.Fatal("o segundo arquivo foi depositado sobre o primeiro")
	}

	// Os dois permanecem recuperáveis, cada um com o seu conteúdo.
	if string(h.objeto(chaveOriginal)) != returnContent {
		t.Error("o conteúdo do primeiro arquivo mudou")
	}
	if string(h.objeto(novaChave)) != outroConteudo {
		t.Error("o conteúdo do segundo arquivo não é o que chegou")
	}
}

// CA3 — nome DIFERENTE, conteúdo IDÊNTICO: só o hash pega, e o envelope diz a qual recepção
// anterior corresponde.
func TestCA5_NomeDiferenteComMesmoConteudoEhReconhecidoPeloHash(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)
	primeiro := h.run()
	chaveOriginal := primeiro.Outcomes[0].StoredKey

	const outroNome = "PAG_000000.20260818130000_0002.RET"
	h.now = h.now.Add(time.Hour)
	h.avancaRelogio()
	h.entrega(outroNome, returnContent, true)
	segundo := h.run()

	if !segundo.Outcomes[0].Duplicate {
		t.Fatal("o mesmo conteúdo com outro nome precisa ser reconhecido pelo hash")
	}
	// Nada de novo foi depositado: uma cópia idêntica sob outra chave só produziria um objeto que
	// ninguém sabe ligar à original.
	if segundo.Outcomes[0].StoredKey != "" {
		t.Errorf("nada devia ter sido depositado; veio %q", segundo.Outcomes[0].StoredKey)
	}
	if h.hasKey(h.prefixes.Returns + outroNome) {
		t.Error("o conteúdo repetido foi depositado sob o nome novo")
	}

	dup := h.envelopeEm(segundo.Outcomes[0].StatusKey)
	if dup.Recepcao == nil || dup.Recepcao.DuplicadoDe != chaveOriginal {
		t.Errorf("o envelope precisa apontar para a recepção anterior (%q); veio %+v", chaveOriginal, dup.Recepcao)
	}
	// O detalhe precisa registrar que o NOME mudou — é o que explica a quem investiga por que um
	// arquivo aparentemente novo não gerou objeto novo.
	if !strings.Contains(dup.Detalhe, returnName) {
		t.Errorf("o detalhe deveria citar o nome da recepção anterior; veio: %q", dup.Detalhe)
	}
}

// CA4 — o envelope carrega o hash em TODOS os desfechos, inclusive no duplicado: é o que permite ao
// core-api decidir sem reabrir o objeto.
func TestCA5_TodoEnvelopeDeRecepcaoCarregaOHash(t *testing.T) {
	h := newReceiveHarness(t)
	h.entrega(returnName, returnContent, true)
	primeiro := h.run()

	h.now = h.now.Add(time.Hour)
	h.avancaRelogio()
	h.entrega(returnName, returnContent, true)
	segundo := h.run()

	for _, key := range []string{primeiro.Outcomes[0].StatusKey, segundo.Outcomes[0].StatusKey} {
		env := h.envelopeEm(key)
		if env.Recepcao == nil || env.Recepcao.Sha256 != sha256Of(returnContent) {
			t.Errorf("envelope em %q não carrega o sha256 do conteúdo: %+v", key, env.Recepcao)
		}
	}
}

// CA5 — os dois índices coexistem. Um indexa nome de remessa, o outro hash de conteúdo; um
// diretório compartilhado deixaria aberta a colisão entre os dois.
func TestCA5_IndiceDeRecepcaoNaoColideComORegistroDeRemessa(t *testing.T) {
	h := newReceiveHarness(t)

	// Uma remessa é transmitida — o registro dela existe.
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))
	if _, err := h.ag.TransmitCycle(context.Background()); err != nil {
		t.Fatalf("ciclo de transmissão: %v", err)
	}

	// E um arquivo é recebido.
	h.entrega(returnName, returnContent, true)
	h.run()

	// Os dois registros continuam íntegros e independentes.
	entry, found, err := h.index.Lookup(sha256Of(returnContent))
	if err != nil || !found {
		t.Fatalf("o registro de recepção sumiu (found=%v, err=%v)", found, err)
	}
	if entry.Arquivo != returnName {
		t.Errorf("o registro de recepção guarda %q, esperava %q", entry.Arquivo, returnName)
	}

	// E a remessa continua sendo tratada como duplicada, ou seja: o registro dela não foi tocado.
	h.store.Seed(h.prefixes.Outbound+remittanceName, []byte(remittanceContent))
	acionamentos := len(h.fake.Calls())
	if _, err := h.ag.TransmitCycle(context.Background()); err != nil {
		t.Fatalf("segundo ciclo de transmissão: %v", err)
	}
	if len(h.fake.Calls()) != acionamentos {
		t.Error("o registro da remessa foi perdido: o cliente foi acionado de novo para o mesmo nome")
	}
}
