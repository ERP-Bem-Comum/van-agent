package envelope_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ERP-Bem-Comum/van-agent/internal/envelope"
)

// -update regrava o golden a partir do código. O arquivo NUNCA é editado à mão: ele é o contrato
// entre dois repositórios, e um golden escrito manualmente pode descrever algo que o produtor não
// produz — que é exatamente a divergência que ele existe para impedir.
var update = flag.Bool("update", false, "regravar testdata/status-envelope.golden.json")

const goldenPath = "../../testdata/status-envelope.golden.json"

// goldenFile é o formato do contrato compartilhado. Cada caso traz a CHAVE e o CORPO, porque o
// consumidor decide o tipo do status pela chave (`classifyKey`) e o conteúdo pelo corpo — um golden
// só com o corpo deixaria metade do contrato sem cobertura.
type goldenFile struct {
	Descricao string       `json:"descricao"`
	Fonte     string       `json:"fonte"`
	Casos     []goldenCase `json:"casos"`
}

type goldenCase struct {
	Nome string `json:"nome"`
	// Tipo é o que o consumidor deve concluir a partir da CHAVE: remittance, duplicate ou reception.
	Tipo string `json:"tipo"`
	// ContaComoTransmissao é o que `wasTransmitted` deve devolver do outro lado. `duplicate` é
	// sempre false, mesmo declarando `transmitido` — e este campo existe para que essa regra seja
	// verificada, e não apenas descrita em comentário.
	ContaComoTransmissao bool              `json:"contaComoTransmissao"`
	Chave                string            `json:"chave"`
	Envelope             envelope.Envelope `json:"envelope"`
}

func intPtr(v int) *int { return &v }

// logLine monta uma linha no layout posicional do §12 (p.30) com as dez larguras corretas, para que
// o golden carregue uma linha REAL em vez de um texto qualquer.
func logLine(op, result, fileName string) string {
	pad := func(s string, w int) string {
		if len(s) > w {
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	padNum := func(s string, w int) string {
		if len(s) > w {
			return s[len(s)-w:]
		}
		return strings.Repeat("0", w-len(s)) + s
	}
	return "20260818120000" + op + pad("PERFIL-DE-TESTE", 30) + pad("STCPCLT", 16) +
		pad("00001234", 8) + pad("00005678", 8) + padNum(result, 6) + padNum("240", 12) +
		pad(fileName, 256) + pad("", 128)
}

// O nome do arquivo de RETORNO segue a convenção do banco, não a nossa: quem o atribui é ele. As
// duas nomenclaturas convivem neste arquivo de propósito.
const returnName = "PAG_000000.20260818110000_0001.RET"

// returnContent é o conteúdo fictício cujo hash aparece no golden. Ele existe para que o hash seja
// VERIFICÁVEL — um valor hexadecimal digitado à mão não provaria que o produtor calcula o que o
// contrato publica.
const returnContent = "0372345...CONTEUDO CNAB DE RETORNO FICTICIO..."

func returnSha256() string {
	sum := sha256.Sum256([]byte(returnContent))
	return hex.EncodeToString(sum[:])
}

func buildGolden() goldenFile {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	name := "PAG_000000.20260818120000_0001.REM"
	returnSum := returnSha256()

	return goldenFile{
		Descricao: "Contrato do prefixo status/ da VAN. Produzido pelo van-agent, consumido pelo core-api " +
			"(src/modules/financial/adapters/van/status-envelope.ts). Gerado por " +
			"`go test ./internal/envelope -update` — não editar à mão.",
		Fonte: "ADR-0061 §2 · manual STCP OFTP Client v5.3 §12 p.30 (layout do log de transferências)",
		Casos: []goldenCase{
			{
				Nome:                 "remessa transmitida",
				Tipo:                 "remittance",
				ContaComoTransmissao: true,
				Chave:                envelope.Key(name),
				Envelope: envelope.New(name, at, envelope.Transmitted,
					"arquivo saiu da pasta de saída e apareceu em backup",
					intPtr(0),
					[]string{
						logLine("0004", "000000", name),
						logLine("0005", "000000", name),
					}),
			},
			{
				Nome:                 "remessa recusada pelo transporte",
				Tipo:                 "remittance",
				ContaComoTransmissao: false,
				Chave:                envelope.Key(name),
				Envelope: envelope.New(name, at, envelope.Failed,
					"arquivo permanece na pasta de saída; nada foi transmitido; código de saída do cliente: 1",
					intPtr(1),
					[]string{
						logLine("0004", "000000", name),
						// 000401 — §11, p.24: nome inválido ou filtro de nomenclatura.
						logLine("0005", "000401", name),
					}),
			},
			{
				// O caso que mais quebra consumidor: exitCode null e log vazio.
				Nome:                 "tentativa duplicada — cliente não acionado",
				Tipo:                 "duplicate",
				ContaComoTransmissao: false,
				Chave:                envelope.DuplicateKey(name, at),
				Envelope: envelope.New(name, at, envelope.Transmitted,
					`nome já processado em 2026-08-18T11:00:00Z com situação "transmitido"; o cliente STCP não foi acionado`,
					nil, nil),
			},
			{
				Nome:                 "execução interrompida — revisão humana",
				Tipo:                 "remittance",
				ContaComoTransmissao: false,
				Chave:                envelope.Key(name),
				Envelope: envelope.New(name, at, envelope.Review,
					"execução anterior foi interrompida após gravar a intenção e antes de registrar o desfecho; "+
						"o arquivo pode ter sido transmitido e NÃO é retransmitido automaticamente — exige conferência humana",
					nil, nil),
			},
			{
				// O caso que era ASPIRACIONAL: o contrato publicava um envelope de recepção que o
				// produtor não produzia. A partir daqui ele é gerado pelo mesmo construtor que o
				// ciclo usa.
				Nome:                 "ciclo de recepção com arquivo recebido",
				Tipo:                 "reception",
				ContaComoTransmissao: false,
				Chave:                envelope.ReceptionKey(returnName, at),
				Envelope: envelope.NewReception(returnName, at, envelope.Reception,
					"arquivo recebido do banco e depositado no prefixo de retorno",
					intPtr(0),
					[]string{logLine("0007", "000000", returnName)},
					envelope.ReceptionInfo{
						Sha256:         returnSum,
						Chave:          "retorno/" + returnName,
						Correlacionado: true,
					}),
			},
			{
				// A caixa é do CONVÊNIO: chegam arquivos de lotes que não são nossos, e nem tudo
				// casa com o log do ciclo. O que não casa entra ASSIM MESMO, declarado — descartar
				// em silêncio um arquivo do banco é o desfecho que ninguém percebe.
				Nome:                 "recepção sem correlação com o log do ciclo",
				Tipo:                 "reception",
				ContaComoTransmissao: false,
				Chave:                envelope.ReceptionKey(returnName, at),
				Envelope: envelope.NewReception(returnName, at, envelope.Reception,
					"arquivo encontrado na pasta de entrada SEM linha correspondente no log deste ciclo; "+
						"depositado assim mesmo, com a origem declarada como não correlacionada",
					intPtr(0), nil,
					envelope.ReceptionInfo{
						Sha256:         returnSum,
						Chave:          "retorno/" + returnName,
						Correlacionado: false,
					}),
			},
		},
	}
}

func TestGoldenDoContratoEstaEmDia(t *testing.T) {
	built := buildGolden()

	raw, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		t.Fatalf("serializar golden: %v", err)
	}
	raw = append(raw, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("preparar diretório do golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, raw, 0o644); err != nil {
			t.Fatalf("gravar golden: %v", err)
		}
		t.Logf("golden regravado em %s", goldenPath)
		return
	}

	onDisk, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("ler golden (rode `go test ./internal/envelope -update`): %v", err)
	}
	if string(onDisk) != string(raw) {
		t.Errorf("o golden divergiu do que o código produz.\n" +
			"Se a mudança é intencional, rode `go test ./internal/envelope -update` E replique o arquivo\n" +
			"no core-api (tests/modules/financial/adapters/van/status-envelope.golden.json) — as duas\n" +
			"metades do contrato precisam mudar juntas.")
	}
}

// O invariante que o consumidor cobra e que um slice nil quebraria em silêncio.
func TestLogVazioSerializaComoArrayNuncaComoNull(t *testing.T) {
	env := envelope.New("X.REM", time.Now(), envelope.Review, "sem log", nil, nil)

	raw, err := envelope.Marshal(env)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if !strings.Contains(string(raw), `"logTransferencia": []`) {
		t.Errorf("logTransferencia precisa sair como array vazio; veio:\n%s", raw)
	}
	if strings.Contains(string(raw), `"logTransferencia": null`) {
		t.Error("logTransferencia saiu como null — o consumidor recusa o envelope inteiro")
	}
}

func TestExitCodeAusenteSerializaComoNull(t *testing.T) {
	env := envelope.New("X.REM", time.Now(), envelope.Transmitted, "d", nil, nil)

	raw, err := envelope.Marshal(env)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if !strings.Contains(string(raw), `"exitCode": null`) {
		t.Errorf("exitCode ausente precisa sair como null, nunca 0; veio:\n%s", raw)
	}
}

func TestChaveDoDuplicadoNuncaColideComAChaveNormal(t *testing.T) {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	name := "PAG_000000.20260818120000_0001.REM"

	if envelope.DuplicateKey(name, at) == envelope.Key(name) {
		t.Fatal("a chave do duplicado sobrescreveria o status original")
	}
	// E precisa continuar sendo classificável como duplicado pelo consumidor, que procura o marcador.
	if !strings.Contains(envelope.DuplicateKey(name, at), ".duplicado-") {
		t.Error("a chave do duplicado perdeu o marcador que o consumidor usa para classificá-la")
	}
}

func TestValidNameRecusaOQueProduziriaChaveAmbigua(t *testing.T) {
	casos := []struct {
		nome  string
		razao string
	}{
		{"", "vazio"},
		{"../etc/passwd", "travessia"},
		{"pasta/arquivo.REM", "separador"},
		{"PAG_1.duplicado-X.REM", "marcador de duplicado viraria status de tentativa recusada"},
		{"recepcao-2026.REM", "prefixo de recepção viraria status de recepção"},
	}
	for _, c := range casos {
		t.Run(c.razao, func(t *testing.T) {
			if err := envelope.ValidName(c.nome); err == nil {
				t.Errorf("nome %q deveria ser recusado (%s)", c.nome, c.razao)
			}
		})
	}
}

func TestValidNameAceitaONomeDeRemessa(t *testing.T) {
	if err := envelope.ValidName("PAG_000000.20260818120000_0001.REM"); err != nil {
		t.Errorf("nome de remessa legítimo foi recusado: %v", err)
	}
}

// O campo de recepção NÃO pode aparecer em envelope de remessa.
//
// O `omitempty` é o que mantém os envelopes de remessa byte a byte iguais aos que o consumidor já
// aceita — a adição fica contida no único caso que precisava dela. Sem ele, todo envelope de
// transmissão passaria a carregar um `"recepcao": null`, e um contrato que muda para todo mundo por
// causa de um caso é exatamente o que o golden existe para pegar.
func TestEnvelopeDeRemessaNaoCarregaOCampoDeRecepcao(t *testing.T) {
	env := envelope.New("PAG_000000_000001.REM", time.Now(), envelope.Transmitted, "d", nil, nil)

	raw, err := envelope.Marshal(env)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if strings.Contains(string(raw), "recepcao") {
		t.Errorf("envelope de remessa carregou o campo de recepção:\n%s", raw)
	}
}

// E o inverso: o envelope de recepção precisa carregar o hash, senão o consumidor teria de reabrir
// o objeto para saber o que recebeu — que é exatamente o que o campo existe para evitar.
func TestEnvelopeDeRecepcaoCarregaOHashDoConteudo(t *testing.T) {
	env := envelope.NewReception(returnName, time.Now(), envelope.Reception, "d", nil, nil,
		envelope.ReceptionInfo{Sha256: returnSha256(), Chave: "retorno/" + returnName, Correlacionado: true})

	raw, err := envelope.Marshal(env)
	if err != nil {
		t.Fatalf("serializar: %v", err)
	}
	if !strings.Contains(string(raw), returnSha256()) {
		t.Errorf("o envelope de recepção precisa carregar o sha256 do conteúdo:\n%s", raw)
	}
	if env.LogTransferencia == nil {
		t.Error("logTransferencia nil serializa como null e faz o consumidor recusar o envelope inteiro")
	}
}

// A chave precisa distinguir dois arquivos recebidos no MESMO instante. Com o carimbo sozinho, o
// segundo apagaria a evidência do primeiro.
func TestChaveDeRecepcaoDistingueArquivosDoMesmoCiclo(t *testing.T) {
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	primeira := envelope.ReceptionKey("A.RET", at)
	segunda := envelope.ReceptionKey("B.RET", at)

	if primeira == segunda {
		t.Fatal("dois arquivos recebidos no mesmo instante colidiriam na mesma chave")
	}
	// E as duas continuam classificáveis como recepção pelo consumidor, que procura o prefixo.
	for _, k := range []string{primeira, segunda} {
		if !strings.HasPrefix(k, envelope.StatusPrefix+"recepcao-") {
			t.Errorf("chave %q perdeu o prefixo que o consumidor usa para classificá-la", k)
		}
	}
}
