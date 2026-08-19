package main

// O que estes testes precisam provar, e o que seria fácil provar por engano.
//
// Fácil e inútil: que o `stcpfake` funciona. Ele já tem a suíte dele, e reexercitá-lo aqui não diz
// nada sobre este binário.
//
// O que importa é o ELO: que a linha de comando que o agente EMITE é a que este programa ENTENDE, e
// que o programa deixa no disco a evidência física que o agente vai LER. É a única costura nova, e é
// exatamente onde dois modelos do mesmo sistema divergem sem ninguém notar.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ERP-Bem-Comum/van-agent/internal/stcp"
)

// ─────────────────────────────────────────────────────────────────────────────
// O elo: o que o agente emite é o que este programa entende
// ─────────────────────────────────────────────────────────────────────────────

// A linha de comando NÃO é montada à mão aqui: ela vem de `stcp.CommandClient.Args`, que é a mesma
// função que o agente usa em produção.
//
// Um teste que escrevesse os argumentos à mão passaria a concordar consigo mesmo — e no dia em que a
// ordem do §6 mudasse de um lado, continuaria verde enquanto a simulação quebrava.
func TestEntendeALinhaDeComandoQueOAgenteEmite(t *testing.T) {
	client, err := stcp.NewCommandClient(stcp.CommandConfig{
		ExecutablePath:       "/caminho/irrelevante/para-o-parse",
		ConfigPath:           "C:\\stcp\\config.ini",
		Profile:              "PERFIL-DE-TESTE",
		Retries:              3,
		RetryIntervalSeconds: 60,
	})
	if err != nil {
		t.Fatalf("montar cliente: %v", err)
	}

	casos := []struct {
		nome   string
		mode   stcp.Mode
		filtro string
	}{
		{"transmissão com filtro", stcp.ModeSend, `^PAG_000000\.REM$`},
		{"transmissão sem filtro", stcp.ModeSend, ""},
		{"recepção", stcp.ModeReceive, ""},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			got, err := parseArgs(client.Args(c.mode, c.filtro))
			if err != nil {
				t.Fatalf("o programa não entendeu a linha que o agente emite: %v\nargs: %q",
					err, client.Args(c.mode, c.filtro))
			}
			if got.Mode != c.mode {
				t.Errorf("modo = %q, esperava %q", got.Mode, c.mode)
			}
			if got.Filter != c.filtro {
				t.Errorf("filtro = %q, esperava %q", got.Filter, c.filtro)
			}
			// O caminho de configuração é POSICIONAL e vem primeiro; se o parse o perdesse, um
			// argumento inteiro estaria sendo ignorado em silêncio.
			if got.ConfigPath != "C:\\stcp\\config.ini" {
				t.Errorf("configuração = %q, esperava o primeiro argumento posicional", got.ConfigPath)
			}
			if got.Profile != "PERFIL-DE-TESTE" {
				t.Errorf("perfil = %q", got.Profile)
			}
		})
	}
}

func TestModoAusenteOuDesconhecidoEhRecusado(t *testing.T) {
	for _, argv := range [][]string{
		{"config.ini", "-p", "P", "-w", "0"},
		{"config.ini", "-m", "X", "-w", "0"},
	} {
		if _, err := parseArgs(argv); err == nil {
			t.Errorf("argv %q deveria ser recusado: sem modo válido não há encenação possível", argv)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// A trava: o programa recusa rodar sem a confirmação
// ─────────────────────────────────────────────────────────────────────────────

// Um falso cliente é perigoso porque PARECE funcionar. Sem a trava, apontar o agente para cá numa
// instalação real publicaria "transmitido" sobre pagamento que nunca saiu.
func TestRecusaRodarSemConfirmacaoExplicita(t *testing.T) {
	h := novaInstalacao(t)

	for _, valor := range []string{"", "1", "true", "sim", "nao-transmite"} {
		t.Setenv(confirmVar, valor)
		var out, errOut strings.Builder
		if code := run(h.argsDeEnvio(""), &out, &errOut); code != exitConfig {
			t.Fatalf("com %s=%q o programa rodou (código %d); a trava não pode ser ligada por acidente",
				confirmVar, valor, code)
		}
		if !strings.Contains(errOut.String(), "NÃO TRANSMITE NADA") {
			t.Error("a recusa precisa dizer o que o programa é; um erro genérico não protege ninguém")
		}
		// E o mundo não pode ter sido tocado.
		if _, err := os.Stat(filepath.Join(h.saida, remessa)); err != nil {
			t.Error("o arquivo saiu da pasta de saída numa execução RECUSADA")
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// A evidência física: é isto que o agente lê para decidir
// ─────────────────────────────────────────────────────────────────────────────

func TestEncenacaoDeSucessoProduzAEvidenciaFisicaQueOAgenteLe(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)

	var out, errOut strings.Builder
	if code := run(h.argsDeEnvio(""), &out, &errOut); code != exitOK {
		t.Fatalf("código = %d, esperava %d · stderr: %s", code, exitOK, errOut.String())
	}

	// §5, p.13 — sai da SAÍDA, aparece em BACKUP. É este par que o agente lê como veredito.
	if _, err := os.Stat(filepath.Join(h.saida, remessa)); !os.IsNotExist(err) {
		t.Error("o arquivo continua na pasta de saída; o agente leria isso como nada transmitido")
	}
	if _, err := os.Stat(filepath.Join(h.backup, remessa)); err != nil {
		t.Errorf("o arquivo não apareceu em backup: %v", err)
	}

	// §12, p.30 — o log posicional, que alimenta o `logTransferencia` do envelope.
	registros := h.logDoCiclo(t)
	if len(registros) == 0 {
		t.Fatal("nenhuma linha no log; o envelope sairia sem evidência de transferência")
	}
	if registros[0].FileName != remessa {
		t.Errorf("a linha do log nomeia %q, esperava %q", registros[0].FileName, remessa)
	}

	// E o programa anuncia o que é, em toda execução.
	if !strings.Contains(out.String(), "ENCENAÇÃO") {
		t.Error("a saída precisa anunciar que é encenação; quem lê o log da simulação tem de saber")
	}
}

// O filtro do `-f` é honrado. Um duplo que o ignorasse esconderia exatamente o defeito que a trava de
// nomenclatura existe para barrar: arquivo indevido na pasta sendo enviado.
func TestFiltroDoAgenteEhHonrado(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)

	intruso := "NAO_E_NOSSO.TXT"
	if err := os.WriteFile(filepath.Join(h.saida, intruso), []byte("intruso"), 0o640); err != nil {
		t.Fatal(err)
	}

	var out, errOut strings.Builder
	if code := run(h.argsDeEnvio("^"+strings.ReplaceAll(remessa, ".", `\.`)+"$"), &out, &errOut); code != exitOK {
		t.Fatalf("código = %d · stderr: %s", code, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(h.saida, intruso)); err != nil {
		t.Error("o arquivo fora do filtro foi transmitido; o `-f` não está sendo respeitado")
	}
	if _, err := os.Stat(filepath.Join(h.backup, remessa)); err != nil {
		t.Error("o arquivo dentro do filtro não foi transmitido")
	}
}

// A recusa deixa o arquivo onde está — é o desfecho que o agente publica como `falha`.
func TestEncenacaoDeRecusaDeixaOArquivoNaFila(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)
	t.Setenv("STCP_ENCENADO_COMPORTAMENTO", "recusa")

	var out, errOut strings.Builder
	if code := run(h.argsDeEnvio(""), &out, &errOut); code == exitOK {
		t.Error("a recusa precisa sair com código diferente de zero")
	}
	if _, err := os.Stat(filepath.Join(h.saida, remessa)); err != nil {
		t.Error("na recusa o arquivo PERMANECE na saída; nada saiu")
	}
	if _, err := os.Stat(filepath.Join(h.backup, remessa)); !os.IsNotExist(err) {
		t.Error("nada podia ter aparecido em backup")
	}
}

// O sumiço é o caso que o fake de 15 linhas do core-api não consegue encenar, e é o que o agente
// precisa tratar como AMBÍGUO — nunca como sucesso.
func TestEncenacaoDeSumicoDeixaOMundoAmbiguo(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)
	t.Setenv("STCP_ENCENADO_COMPORTAMENTO", "sumico")

	var out, errOut strings.Builder
	run(h.argsDeEnvio(""), &out, &errOut)

	if _, err := os.Stat(filepath.Join(h.saida, remessa)); !os.IsNotExist(err) {
		t.Error("no sumiço o arquivo sai da pasta de saída")
	}
	if _, err := os.Stat(filepath.Join(h.backup, remessa)); !os.IsNotExist(err) {
		t.Error("no sumiço o arquivo NÃO aparece em backup — é isso que torna o desfecho ambíguo")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Recepção — o modo que ainda não tinha sido exercitado fora da suíte
// ─────────────────────────────────────────────────────────────────────────────

func TestRecepcaoEntregaNaPastaDeEntradaEDeixaLinhaNoLog(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)

	const retorno = "PAG_000000.20260818110000_0001.RET"
	h.enfileiraEntrega(t, retorno, "conteúdo de retorno fictício")

	var out, errOut strings.Builder
	if code := run(h.argsDeRecepcao(), &out, &errOut); code != exitOK {
		t.Fatalf("código = %d · stderr: %s", code, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(h.entrada, retorno)); err != nil {
		t.Fatalf("o arquivo não foi entregue na pasta de ENTRADA: %v", err)
	}
	registros := h.logDoCiclo(t)
	if len(registros) == 0 || registros[0].FileName != retorno {
		t.Error("a entrega precisa deixar linha de recepção no log; sem ela o envelope sai não correlacionado")
	}
}

// Entregar UMA vez só. O cliente real não reentrega o que já entregou — e um duplo que reentregasse
// esconderia a diferença entre "o banco reenviou" e "ninguém tirou o arquivo da pasta".
func TestEntregaNaoSeRepeteNoCicloSeguinte(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)

	const retorno = "PAG_000000.20260818110000_0001.RET"
	h.enfileiraEntrega(t, retorno, "conteúdo")

	var out, errOut strings.Builder
	run(h.argsDeRecepcao(), &out, &errOut)

	// O agente arquiva o que recebeu: some da pasta de entrada.
	if err := os.Remove(filepath.Join(h.entrada, retorno)); err != nil {
		t.Fatalf("preparar o segundo ciclo: %v", err)
	}

	run(h.argsDeRecepcao(), &out, &errOut)

	if _, err := os.Stat(filepath.Join(h.entrada, retorno)); !os.IsNotExist(err) {
		t.Error("o mesmo arquivo foi entregue de novo; o banco não reentrega o que já entregou")
	}
}

// A entrega sem log é o que torna a não-correlação exercitável de propósito — e é o cenário que o
// core-api precisa reproduzir para provar a decisão de quarentena.
func TestEntregaSemLogProduzArquivoSemLinhaDeRecepcao(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)

	const retorno = "PAG_000000.20260818110000_0009.RET"
	h.enfileiraEntrega(t, retorno, "chegou sem o log explicar")
	t.Setenv("STCP_ENCENADO_ENTREGAR_SEM_LOG", `\.RET$`)

	var out, errOut strings.Builder
	if code := run(h.argsDeRecepcao(), &out, &errOut); code != exitOK {
		t.Fatalf("código = %d · stderr: %s", code, errOut.String())
	}

	if _, err := os.Stat(filepath.Join(h.entrada, retorno)); err != nil {
		t.Fatal("o arquivo precisa ser entregue mesmo sem linha de log")
	}
	if registros := h.logDoCiclo(t); len(registros) != 0 {
		t.Errorf("não podia haver linha de log para esta entrega; vieram %d", len(registros))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Ambiente incompleto falha no BOOT, não no meio
// ─────────────────────────────────────────────────────────────────────────────

// A cobrança é por MODO: exigir a pasta de entrada de quem só transmite faria a simulação falhar por
// algo que ela não usa.
func TestAmbienteIncompletoFalhaComAsVariaveisNomeadas(t *testing.T) {
	h := novaInstalacao(t)
	t.Setenv(confirmVar, confirmValue)
	t.Setenv("STCP_ENCENADO_INBOUND_DIR", "")
	t.Setenv("STCP_ENCENADO_ENTREGAR_DE", "")

	var out, errOut strings.Builder
	// Transmissão continua funcionando sem as pastas de recepção.
	if code := run(h.argsDeEnvio(""), &out, &errOut); code != exitOK {
		t.Errorf("a transmissão não devia exigir pastas de recepção; código %d · %s", code, errOut.String())
	}

	errOut.Reset()
	if code := run(h.argsDeRecepcao(), &out, &errOut); code != exitConfig {
		t.Errorf("a recepção sem pasta de entrada devia falhar na configuração; veio %d", code)
	}
	if !strings.Contains(errOut.String(), "STCP_ENCENADO_INBOUND_DIR") {
		t.Errorf("o erro precisa NOMEAR a variável ausente; quem lê está numa máquina, sem o código: %s",
			errOut.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Instalação simulada
// ─────────────────────────────────────────────────────────────────────────────

const remessa = "PAG_000000.20260818120000_0001.REM"

type instalacao struct {
	saida, backup, entrada, entregarDe, logPath string
}

// novaInstalacao monta as pastas em disco REAL, como o resto da suíte do repositório: a evidência
// que o agente lê é o sistema de arquivos, e um duplo de disco tornaria a afirmação circular.
func novaInstalacao(t *testing.T) *instalacao {
	t.Helper()
	root := t.TempDir()

	h := &instalacao{
		saida:      filepath.Join(root, "SAIDA"),
		backup:     filepath.Join(root, "BACKUP"),
		entrada:    filepath.Join(root, "ENTRADA"),
		entregarDe: filepath.Join(root, "ENTREGAR"),
		logPath:    filepath.Join(root, "LOG", "20260818.LOG"),
	}
	for _, d := range []string{h.saida, h.backup, h.entrada, h.entregarDe, filepath.Dir(h.logPath)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("preparar %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.saida, remessa), []byte("conteúdo fictício"), 0o640); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STCP_ENCENADO_OUTBOUND_DIR", h.saida)
	t.Setenv("STCP_ENCENADO_BACKUP_DIR", h.backup)
	t.Setenv("STCP_ENCENADO_INBOUND_DIR", h.entrada)
	t.Setenv("STCP_ENCENADO_ENTREGAR_DE", h.entregarDe)
	t.Setenv("STCP_ENCENADO_LOG_PATH", h.logPath)
	return h
}

func (h *instalacao) argsDeEnvio(filtro string) []string {
	args := []string{"config.ini", "-p", "PERFIL-DE-TESTE", "-r", "3", "-t", "60", "-m", "S"}
	if filtro != "" {
		args = append(args, "-f", filtro)
	}
	return append(args, "-w", "0")
}

func (h *instalacao) argsDeRecepcao() []string {
	return []string{"config.ini", "-p", "PERFIL-DE-TESTE", "-r", "3", "-t", "60", "-m", "R", "-w", "0"}
}

func (h *instalacao) enfileiraEntrega(t *testing.T, nome, conteudo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.entregarDe, nome), []byte(conteudo), 0o640); err != nil {
		t.Fatalf("enfileirar entrega: %v", err)
	}
}

// logDoCiclo decodifica o log com o MESMO parser que o agente usa. Um teste que lesse o arquivo por
// conta própria poderia concordar com um layout que o agente não entende.
func (h *instalacao) logDoCiclo(t *testing.T) []stcp.Record {
	t.Helper()
	raw, err := os.ReadFile(h.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ler log: %v", err)
	}
	return stcp.ParseLog(string(raw))
}
