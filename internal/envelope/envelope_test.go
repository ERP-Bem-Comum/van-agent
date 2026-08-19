package envelope_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
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
						LogDoCicloLido: true,
					}),
			},
			{
				// A caixa é do CONVÊNIO: chegam arquivos de lotes que não são nossos, e nem tudo
				// casa com o log do ciclo. O que não casa entra ASSIM MESMO, declarado — descartar
				// em silêncio um arquivo do banco é o desfecho que ninguém percebe.
				//
				// Aqui o log DESTE ciclo foi lido: a ausência da linha é afirmação sobre o arquivo,
				// e este é o único dos dois casos de não-correlação que sustenta uma suspeita.
				Nome:                 "recepção sem correlação, com o log do ciclo lido",
				Tipo:                 "reception",
				ContaComoTransmissao: false,
				Chave:                envelope.ReceptionKey(returnName, at),
				Envelope: envelope.NewReception(returnName, at, envelope.Reception,
					"arquivo encontrado na pasta de entrada SEM linha correspondente no log deste "+
						"ciclo, que FOI lido; depositado assim mesmo, com a origem declarada como não "+
						"correlacionada — o cliente não registrou tê-lo recebido nesta execução",
					intPtr(0), nil,
					envelope.ReceptionInfo{
						Sha256:         returnSum,
						Chave:          "retorno/" + returnName,
						Correlacionado: false,
						LogDoCicloLido: true,
					}),
			},
			{
				// O MESMO `correlacionado: false` do caso acima, com significado oposto — e é por
				// isso que `logDoCicloLido` existe.
				//
				// O nome do log começa por data (§7, p.15) e o padrão casa o mais recente: no
				// primeiro ciclo do dia, antes de o cliente escrever o log novo, o agente lê com
				// sucesso o log de ONTEM. Sem este campo o consumidor não teria como separar isto
				// de uma origem não registrada, e represaria todo retorno do primeiro ciclo,
				// diariamente. Aqui a não-correlação não diz nada sobre o arquivo: diz que a
				// configuração do log precisa ser conferida.
				Nome:                 "recepção sem o log do ciclo — o agente não sabe",
				Tipo:                 "reception",
				ContaComoTransmissao: false,
				Chave:                envelope.ReceptionKey(returnName, at),
				Envelope: envelope.NewReception(returnName, at, envelope.Reception,
					"arquivo encontrado na pasta de entrada e depositado, mas o log DESTA execução "+
						"não pôde ser lido (padrão sem correspondência, log ainda não escrito ou "+
						"leitura que falhou); a ausência de correlação NÃO é indício sobre o arquivo "+
						"— é sobre a configuração do log na instalação, e é ela que precisa ser conferida",
					intPtr(0), nil,
					envelope.ReceptionInfo{
						Sha256:         returnSum,
						Chave:          "retorno/" + returnName,
						Correlacionado: false,
						LogDoCicloLido: false,
					}),
			},
			{
				// O mesmo CONTEÚDO reaparecendo. Reconhecido pelo hash, nunca pelo nome: o nome é
				// atribuído pelo banco, o mesmo arquivo pode voltar com nome diferente, e nomes
				// iguais podem trazer conteúdo diferente.
				//
				// `chave` aponta para a recepção ORIGINAL, e é a mesma de `duplicadoDe`: nada novo
				// foi depositado, e o consumidor precisa saber a qual objeto este envelope se refere.
				Nome:                 "recepção duplicada — mesmo conteúdo já recebido",
				Tipo:                 "reception",
				ContaComoTransmissao: false,
				Chave:                envelope.ReceptionKey(returnName, at),
				Envelope: envelope.NewReception(returnName, at, envelope.Reception,
					"conteúdo idêntico a uma recepção anterior (mesmo sha256), já depositado em "+
						`"retorno/`+returnName+`" em 2026-08-18T11:00:00Z; o objeto anterior NÃO foi `+
						"sobrescrito e nada foi depositado de novo",
					nil, nil,
					envelope.ReceptionInfo{
						Sha256:         returnSum,
						Chave:          "retorno/" + returnName,
						Correlacionado: true,
						LogDoCicloLido: true,
						Duplicado:      true,
						DuplicadoDe:    "retorno/" + returnName,
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

// ─────────────────────────────────────────────────────────────────────────────
// A chave de remessa é função SÓ do nome — e o consumidor depende disso
// ─────────────────────────────────────────────────────────────────────────────

// Por que estes testes existem, e por que a expectativa é montada à mão.
//
// O core-api resolve conflito entre dois envelopes do mesmo arquivo pela ordem LEXICOGRÁFICA da
// chave: vence o primeiro, `executadoEm` não é consultado, e o agregado recusa a segunda mudança em
// qualquer direção — inclusive promoção de falha para sucesso (medido por eles em 19/08). Isso é
// seguro hoje por uma razão que mora AQUI: `Key` não recebe relógio, então não há como o mesmo nome
// produzir duas chaves de remessa.
//
// Se alguém adicionar carimbo a `Key` — pedido plausível, para parar de sobrescrever histórico —, a
// correção do outro lado quebra em SILÊNCIO, do outro lado da fronteira. Estes testes transformam
// isso em vermelho aqui.
//
// A chave esperada é escrita LITERALMENTE, sem chamar `Key`, de propósito: um teste que montasse a
// expectativa com a própria função acompanharia a mudança e concordaria com o defeito.

// A trava mais forte é de COMPILAÇÃO, não de asserção.
//
// `Key` recebe um nome e devolve uma chave, e nada mais. Se ela ganhar um segundo parâmetro — um
// relógio, um contador de tentativa, um sufixo —, esta linha para de compilar. Diferente de uma
// asserção, isso não pode ser "atualizado junto" por quem está fazendo a mudança sem parar para
// pensar: o build quebra antes de qualquer teste rodar.
//
// As outras duas recebem relógio de propósito, e a assimetria é o contrato: só as chaves que
// PRECISAM não colidir carregam tempo.
var (
	_ func(string) string            = envelope.Key
	_ func(string, time.Time) string = envelope.DuplicateKey
	_ func(string, time.Time) string = envelope.ReceptionKey
)

func TestChaveDeRemessaEhFuncaoSoDoNome(t *testing.T) {
	const nome = "PAG_000000.20260818120000_0001.REM"

	if got, want := envelope.Key(nome), "status/"+nome+".json"; got != want {
		t.Fatalf("Key(%q) = %q, esperava %q — a chave de remessa deriva SÓ do nome", nome, got, want)
	}
}

// A asserção que pega a regressão de verdade: nenhum carimbo dentro da chave de remessa.
//
// `StampLayout` renderiza como 8 dígitos, `T`, 6 dígitos, `Z`. Se um carimbo entrar na chave — por
// qualquer motivo, com qualquer formato desta família — esta asserção cai, mesmo que a igualdade
// literal acima seja atualizada junto pela mesma mão.
func TestChaveDeRemessaNaoCarregaCarimbo(t *testing.T) {
	carimbo := regexp.MustCompile(`[0-9]{8}T[0-9]{6}Z`)

	for _, nome := range []string{
		"PAG_000000.20260818120000_0001.REM",
		"PAG_000000_000042.REM",
		"X.REM",
	} {
		key := envelope.Key(nome)
		// O nome pode conter dígitos e ponto, mas nunca a forma do carimbo — que é o que o
		// consumidor usaria para distinguir duas chaves do mesmo arquivo.
		if carimbo.MatchString(strings.TrimPrefix(strings.TrimSuffix(key, ".json"), "status/"+nome)) {
			t.Errorf("a chave de remessa de %q ganhou carimbo: %q — o consumidor passa a ter duas "+
				"chaves para o mesmo arquivo, e ele decide por ordem lexicográfica", nome, key)
		}
	}

	// E as outras duas famílias carregam carimbo de propósito: é o que as torna distintas entre si e
	// da chave de remessa. Se ESTAS perderem o carimbo, um duplicado passa a sobrescrever o original.
	at := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for nome, key := range map[string]string{
		"duplicado": envelope.DuplicateKey("X.REM", at),
		"recepção":  envelope.ReceptionKey("X.RET", at),
	} {
		if !carimbo.MatchString(key) {
			t.Errorf("a chave de %s perdeu o carimbo (%q); sem ele ela colide e apaga a anterior", nome, key)
		}
	}
}
