package spool

// O padrão do log e a caixa (#17).
//
// Testes no pacote INTERNO de propósito: o que precisa ser exercitado é a decisão por regime de
// caixa, e ela é interna. Um teste externo só conseguiria exercitar o regime da máquina em que a
// suíte roda — e o defeito mora justamente no OUTRO regime, o do Windows, onde ninguém roda a suíte.

import (
	"os"
	"path/filepath"
	"testing"
)

// A afirmação que motiva tudo: o Go não faz case folding em lugar nenhum.
//
// Se um dia a biblioteca padrão mudar isso, este teste cai — e a mudança inteira do #17 passa a ser
// desnecessária. Vale mais como registro do que como proteção.
func TestFilepathMatchDoGoIgnoraOFilesystemEEhSensivelACaixa(t *testing.T) {
	for _, c := range []struct{ padrao, nome string }{
		{"*.LOG", "20260818.log"},
		{"*.log", "20260818.LOG"},
		{"PAG*.LOG", "pag_teste.log"},
	} {
		if ok, _ := filepath.Match(c.padrao, c.nome); ok {
			t.Errorf("filepath.Match(%q, %q) casou; o pressuposto do #17 mudou", c.padrao, c.nome)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Windows: a caixa NÃO distingue — o agente concorda com a plataforma
// ─────────────────────────────────────────────────────────────────────────────

func TestOndeACaixaNaoImportaOPadraoCasaIndependenteDela(t *testing.T) {
	dir := comArquivos(t, "20260818.log")

	for _, padrao := range []string{"*.LOG", "*.log", "*.LoG", "202608*.LOG"} {
		got, err := casarLogs(dir, padrao, false)
		if err != nil {
			t.Fatalf("padrão %q: %v", padrao, err)
		}
		if len(got) != 1 {
			t.Errorf("padrão %q não casou o log (%d resultados); no Windows quem configura não tem "+
				"motivo para pensar em caixa, e a correlação nunca funcionaria", padrao, len(got))
		}
	}
}

// O caso real da instalação: o operador escreve o padrão em maiúsculas porque é como o manual e o
// Explorer mostram, e o cliente grava o arquivo em minúsculas.
func TestOndeACaixaNaoImportaOLogDoClienteEhEncontrado(t *testing.T) {
	dir := comArquivos(t, "stcp_transfer.log")

	got, err := casarLogs(dir, "*.LOG", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("o log não foi encontrado; é este o caminho que produz logDoCicloLido:false em todo "+
			"retorno, sem erro nenhum. Resultados: %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Linux/macOS: a caixa distingue — e o comportamento NÃO muda
// ─────────────────────────────────────────────────────────────────────────────

// A garantia principal do regime estrito, e ela roda em QUALQUER filesystem.
//
// Um único arquivo, padrão com a caixa trocada: onde a caixa importa, não casa. Este teste não
// depende de o sistema conseguir criar dois nomes que diferem só na caixa — e é por isso que ele
// existe separado do de baixo, que precisa disso e pula no macOS.
func TestOndeACaixaImportaPadraoComCaixaTrocadaNaoCasa(t *testing.T) {
	dir := comArquivos(t, "20260818.log")

	got, err := casarLogs(dir, "*.LOG", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("onde a caixa importa, %q não pode casar %q; veio %v", "*.LOG", "20260818.log", got)
	}

	// E o mesmo diretório, no regime do Windows, casa — é a diferença inteira do #17 em duas linhas.
	got, err = casarLogs(dir, "*.LOG", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("onde a caixa NÃO importa, %q precisa casar %q; veio %v", "*.LOG", "20260818.log", got)
	}
}

// Afrouxar aqui seria o oposto de errar para menos: onde o filesystem distingue caixa, dois nomes
// que só diferem nela são DOIS arquivos, e casar ambos escolheria um por acidente de ordenação.
func TestOndeACaixaImportaOPadraoContinuaEstrito(t *testing.T) {
	dir := comArquivos(t, "20260818.log", "20260818.LOG")

	got, err := casarLogs(dir, "*.LOG", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("esperava só o arquivo em maiúsculas, vieram %d: %v", len(got), got)
	}
	if filepath.Base(got[0]) != "20260818.LOG" {
		t.Errorf("casou %q; onde a caixa importa, o padrão precisa continuar distinguindo", got[0])
	}
}

// E o mesmo cenário no outro regime casa os DOIS — que é o comportamento correto do Windows, onde
// esses dois arquivos nem poderiam coexistir.
func TestOsDoisRegimesDivergemNoMesmoDiretorio(t *testing.T) {
	dir := comArquivos(t, "20260818.log", "20260818.LOG")

	estrito, err := casarLogs(dir, "*.LOG", true)
	if err != nil {
		t.Fatal(err)
	}
	frouxo, err := casarLogs(dir, "*.LOG", false)
	if err != nil {
		t.Fatal(err)
	}

	if len(estrito) == len(frouxo) {
		t.Fatalf("os dois regimes deram o mesmo resultado (%d); a distinção não está sendo aplicada",
			len(estrito))
	}
	if len(estrito) != 1 || len(frouxo) != 2 {
		t.Errorf("estrito=%v frouxo=%v", estrito, frouxo)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// O que não muda, e o que vira erro
// ─────────────────────────────────────────────────────────────────────────────

// Subdiretório dentro da pasta de log não é log. O `filepath.Glob` anterior também os traria, e ler
// um diretório como se fosse arquivo produziria erro no meio do ciclo em vez de "não há log".
func TestDiretorioNaoEhTratadoComoLog(t *testing.T) {
	dir := comArquivos(t, "20260818.LOG")
	if err := os.MkdirAll(filepath.Join(dir, "antigos.LOG"), 0o750); err != nil {
		t.Fatal(err)
	}

	got, err := casarLogs(dir, "*.LOG", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "20260818.LOG" {
		t.Errorf("um diretório entrou como log: %v", got)
	}
}

// Pasta ausente é "nenhum log", não erro: o log é diagnóstico e o ciclo não pode parar por causa
// dele. Quem cobra a existência das pastas é o boot.
func TestPastaDeLogAusenteNaoEhErro(t *testing.T) {
	got, err := casarLogs(filepath.Join(t.TempDir(), "nao-existe"), "*.LOG", true)
	if err != nil {
		t.Errorf("pasta ausente não pode virar erro: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("esperava nenhum resultado, veio %v", got)
	}
}

// Padrão malformado precisa virar ERRO, e não "não casou nada" — que o chamador leria como ausência
// de log, e publicaria `logDoCicloLido: false` sobre um defeito de configuração que tem conserto.
func TestPadraoMalformadoViraErroEmVezDeSilencio(t *testing.T) {
	dir := comArquivos(t, "20260818.LOG")

	if _, err := casarLogs(dir, "[", true); err == nil {
		t.Error("padrão inválido precisa falhar; em silêncio ele vira 'não há log' para sempre")
	}
}

// ─────────────────────────────────────────────────────────────────────────────

// comArquivos monta uma pasta de log com os arquivos informados, vazios.
//
// ⚠️ Em sistemas de arquivos que não distinguem caixa (macOS por default, Windows), pedir dois nomes
// que só diferem na caixa cria UM arquivo. Os testes que dependem dos dois pulam nesse caso, em vez
// de falhar por um motivo que não é o código.
func comArquivos(t *testing.T, nomes ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range nomes {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o640); err != nil {
			t.Fatalf("criar %q: %v", n, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(nomes) {
		t.Skipf("o sistema de arquivos desta máquina não distingue caixa (%d de %d arquivos criados); "+
			"este caso é exercitado no CI, que roda em Linux", len(entries), len(nomes))
	}
	return dir
}

// ─────────────────────────────────────────────────────────────────────────────
// O cliente ARQUIVA RENOMEANDO (medido na instalação em 27/08/2026)
// ─────────────────────────────────────────────────────────────────────────────

// Este bloco existe por causa de um defeito que chegou a produção: `InBackup` procurava o nome
// EXATO, o cliente arquiva com carimbo, e por isso `transmitido` era inalcançável — duas remessas
// que o banco aceitou saíram como `revisao`. A suíte não pegou porque o duplo movia com o nome
// idêntico, fiel a um manual que não documenta o rename (§5, p.13).

func comBackup(t *testing.T, nomes ...string) *Dir {
	t.Helper()
	d, err := NewDir(Config{
		OutboundDir:     t.TempDir(),
		BackupDir:       comArquivos(t, nomes...),
		LogDir:          t.TempDir(),
		TransferLogGlob: "*.log.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const remessa = "PAG_000000.20260818120000_000001.REM"

// As duas larguras medidas na instalação precisam ser reconhecidas. Cravar uma delas deixaria a
// outra de fora, e não há nada que garanta que existam só duas.
func TestInBackupReconheceOArquivamentoCarimbado(t *testing.T) {
	for _, c := range []struct{ nome, arquivado string }{
		{"carimbo de 17 dígitos, como o BACKUP da transmissão", remessa + ".20260818120005822"},
		{"carimbo de 18 dígitos, como o timestamp OFTP", remessa + ".202608181200050000"},
		{"sem carimbo, para a instalação que não renomeie (§8)", remessa},
	} {
		t.Run(c.nome, func(t *testing.T) {
			ok, err := comBackup(t, c.arquivado).InBackup(remessa)
			if err != nil {
				t.Fatalf("InBackup: %v", err)
			}
			if !ok {
				t.Errorf("%q não foi reconhecido como o arquivamento de %q; o desfecho cairia no ramo "+
					"ambíguo e a remessa sairia como `revisao`", c.arquivado, remessa)
			}
		})
	}
}

// O reconhecimento é ESTRITO, e a razão é a assimetria de custo: um falso negativo devolve
// `revisao`, que é caro mas seguro; um falso positivo afirma `transmitido` sobre um arquivo que não
// saiu, e aí ninguém reenvia. Estes são os casos que um `HasPrefix` cru aceitaria.
func TestInBackupRecusaSufixoQueNaoEhCarimbo(t *testing.T) {
	for _, c := range []struct{ nome, arquivado string }{
		{"sufixo alfabético", remessa + ".backup"},
		{"dígitos de menos para uma data", remessa + ".123"},
		{"dígitos suficientes, data impossível", remessa + ".99999999999999999"},
		{"dígitos com letra no meio", remessa + ".2026081812000x822"},
		{"outra remessa que compartilha o prefixo", remessa + "2.20260818120005822"},
	} {
		t.Run(c.nome, func(t *testing.T) {
			ok, err := comBackup(t, c.arquivado).InBackup(remessa)
			if err != nil {
				t.Fatalf("InBackup: %v", err)
			}
			if ok {
				t.Errorf("%q foi aceito como arquivamento de %q; isso publicaria `transmitido` sobre "+
					"uma remessa que pode não ter saído", c.arquivado, remessa)
			}
		})
	}
}

// Backup vazio e backup ausente são "não está lá", nunca erro: o desfecho de um pagamento não pode
// depender de um `os.Stat` a mais, e quem cobra a existência das pastas é o boot.
func TestInBackupSemOArquivoNaoEhErro(t *testing.T) {
	vazio := comBackup(t)
	if ok, err := vazio.InBackup(remessa); ok || err != nil {
		t.Errorf("backup vazio: ok=%v err=%v; esperado false, nil", ok, err)
	}

	ausente, err := NewDir(Config{
		OutboundDir:     t.TempDir(),
		BackupDir:       filepath.Join(t.TempDir(), "nao-existe"),
		LogDir:          t.TempDir(),
		TransferLogGlob: "*.log.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ausente.InBackup(remessa); ok || err != nil {
		t.Errorf("backup ausente: ok=%v err=%v; esperado false, nil", ok, err)
	}
}

// A caixa segue o regime do sistema, como já faz o padrão do log (#17). No Windows — a plataforma
// de destino, onde ninguém roda a suíte — dois nomes que só diferem na caixa são o MESMO arquivo, e
// exigir caixa exata deixaria o arquivamento sem ser reconhecido.
func TestInBackupSegueORegimeDeCaixaDoSistema(t *testing.T) {
	original := caixaImportaNoSistema
	t.Cleanup(func() { caixaImportaNoSistema = original })

	arquivado := "pag_000000.20260818120000_000001.rem.20260818120005822"

	caixaImportaNoSistema = false
	if ok, err := comBackup(t, arquivado).InBackup(remessa); err != nil || !ok {
		t.Errorf("onde a caixa não importa, %q deveria casar %q (ok=%v err=%v)", arquivado, remessa, ok, err)
	}

	caixaImportaNoSistema = true
	if ok, err := comBackup(t, arquivado).InBackup(remessa); err != nil || ok {
		t.Errorf("onde a caixa importa, %q NÃO deveria casar %q (ok=%v err=%v)", arquivado, remessa, ok, err)
	}
}
