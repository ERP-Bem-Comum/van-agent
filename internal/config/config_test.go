package config_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ERP-Bem-Comum/van-agent/internal/bucket"
	"github.com/ERP-Bem-Comum/van-agent/internal/config"
)

// as variáveis que este pacote lê, todas, para que cada caso comece de um ambiente limpo em vez de
// herdar o que a máquina de quem roda a suíte tiver exportado.
var todasAsVariaveis = []string{
	"VAN_AGENT_LEDGER_DIR",
	"VAN_AGENT_NAME_PATTERN",
	"VAN_AGENT_NAME_MAX_LENGTH",
	"VAN_AGENT_STCP_OUTBOUND_DIR",
	"VAN_AGENT_STCP_BACKUP_DIR",
	"VAN_AGENT_STCP_LOG_DIR",
	"VAN_AGENT_STCP_TRANSFER_LOG_GLOB",
	"VAN_AGENT_STCP_EXE",
	"VAN_AGENT_STCP_INI",
	"VAN_AGENT_STCP_PROFILE",
	"VAN_AGENT_STCP_RETRIES",
	"VAN_AGENT_STCP_RETRY_INTERVAL_SECONDS",
	"VAN_S3_BUCKET",
	"VAN_S3_REGION",
	"VAN_S3_ENDPOINT",
	"VAN_S3_FORCE_PATH_STYLE",
	"VAN_S3_ACCESS_KEY_ID",
	"VAN_S3_SECRET_ACCESS_KEY",
	"VAN_S3_PREFIX_OUTBOUND",
	"VAN_S3_PREFIX_PROCESSED",
	"VAN_S3_PREFIX_FAILED",
	"VAN_S3_PREFIX_RETURNS",
	"VAN_S3_PREFIX_STATUS",
}

func limpaAmbiente(t *testing.T) {
	t.Helper()
	for _, v := range todasAsVariaveis {
		t.Setenv(v, "")
	}
}

// ambienteDaMaquina preenche o que descreve a instalação — nada aqui toca o bucket. Todos os
// valores são fictícios: este repositório é público.
func ambienteDaMaquina(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"VAN_AGENT_LEDGER_DIR":        t.TempDir(),
		"VAN_AGENT_NAME_PATTERN":      `^PAG_\d+_\d+\.REM$`,
		"VAN_AGENT_STCP_OUTBOUND_DIR": t.TempDir(),
		"VAN_AGENT_STCP_BACKUP_DIR":   t.TempDir(),
		"VAN_AGENT_STCP_LOG_DIR":      t.TempDir(),
		"VAN_AGENT_STCP_EXE":          "C:\\fictício\\stcpclt.exe",
		"VAN_AGENT_STCP_INI":          "C:\\fictício\\stcp.ini",
		"VAN_AGENT_STCP_PROFILE":      "PERFIL-DE-TESTE",
	} {
		t.Setenv(k, v)
	}
}

func ambienteDoBucket(t *testing.T) {
	t.Helper()
	t.Setenv("VAN_S3_BUCKET", "bucket-ficticio")
	t.Setenv("VAN_S3_REGION", "us-east-1")
}

// ─────────────────────────────────────────────────────────────────────────────
// CA2 — variável obrigatória ausente: o erro NOMEIA a que falta
// ─────────────────────────────────────────────────────────────────────────────

func TestCA2_BucketOuRegiaoAusenteFalhaNomeandoAVariavel(t *testing.T) {
	casos := []struct {
		remover string
		manter  map[string]string
	}{
		{remover: "VAN_S3_REGION", manter: map[string]string{"VAN_S3_BUCKET": "bucket-ficticio"}},
		{remover: "VAN_S3_BUCKET", manter: map[string]string{"VAN_S3_REGION": "us-east-1"}},
	}

	for _, c := range casos {
		t.Run(c.remover, func(t *testing.T) {
			limpaAmbiente(t)
			for k, v := range c.manter {
				t.Setenv(k, v)
			}

			_, err := config.LoadStorage()
			if err == nil {
				t.Fatalf("configuração sem %s deveria falhar no boot", c.remover)
			}
			// Quem lê este erro está numa máquina Windows, sem o código à mão: a mensagem precisa
			// dizer QUAL variável falta, não apenas que algo falta.
			if !strings.Contains(err.Error(), c.remover) {
				t.Errorf("o erro deveria nomear %s; veio: %v", c.remover, err)
			}
		})
	}
}

// O ensaio precisa rodar numa instalação que ainda não tem bucket: se `Load` exigisse as variáveis
// do armazenamento, o modo que existe para verificar instalação nova seria o primeiro a falhar.
func TestLoadNaoExigeVariavelDeBucket(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)

	if _, err := config.Load(); err != nil {
		t.Fatalf("a configuração da máquina deveria bastar para o ensaio; veio: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA3 — credencial pela metade é erro (XOR), nunca tentativa de autenticar
// ─────────────────────────────────────────────────────────────────────────────

func TestCA3_CredencialPelaMetadeFalhaNomeandoAChaveQueFalta(t *testing.T) {
	casos := []struct {
		nome      string
		informada string
		valor     string
		esperada  string
	}{
		{"só a chave de acesso", "VAN_S3_ACCESS_KEY_ID", "AKIAFICTICIO", "VAN_S3_SECRET_ACCESS_KEY"},
		{"só o segredo", "VAN_S3_SECRET_ACCESS_KEY", "segredo-ficticio", "VAN_S3_ACCESS_KEY_ID"},
	}

	for _, c := range casos {
		t.Run(c.nome, func(t *testing.T) {
			limpaAmbiente(t)
			ambienteDoBucket(t)
			t.Setenv(c.informada, c.valor)

			_, err := config.LoadStorage()
			if err == nil {
				t.Fatal("credencial pela metade deveria falhar no boot, não virar tentativa de autenticar")
			}
			if !strings.Contains(err.Error(), c.esperada) {
				t.Errorf("o erro deveria nomear %s; veio: %v", c.esperada, err)
			}
			// E o valor informado não pode vazar junto com a reclamação.
			if strings.Contains(err.Error(), c.valor) {
				t.Errorf("o erro carregou o valor da credencial: %v", err)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA4 — sem chave nenhuma: cadeia de provedores, e nada de credencial em log
// ─────────────────────────────────────────────────────────────────────────────

func TestCA4_SemChaveAResolucaoCaiNaCadeiaDeProvedores(t *testing.T) {
	limpaAmbiente(t)
	ambienteDoBucket(t)

	cfg, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("configuração sem credencial estática é o caminho de produção; veio: %v", err)
	}
	if cfg.Credentials != nil {
		t.Errorf("sem chave informada, a credencial precisa ficar nil (role da instância); veio %v", cfg.Credentials)
	}
}

func TestCA4_ImprimirAConfiguracaoNaoVazaOSegredo(t *testing.T) {
	limpaAmbiente(t)
	ambienteDoBucket(t)
	t.Setenv("VAN_S3_ACCESS_KEY_ID", "AKIAFICTICIO")
	t.Setenv("VAN_S3_SECRET_ACCESS_KEY", "segredo-que-nao-pode-aparecer")

	cfg, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("credencial completa deveria ser aceita: %v", err)
	}

	// O modo de vazamento real não é alguém logar a credencial de propósito — é alguém depurar um
	// erro de boot com `%v` sobre a configuração inteira, numa máquina cujo log ninguém rotaciona.
	for _, formatado := range []string{
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%+v", cfg),
		fmt.Sprintf("%s", cfg),
	} {
		if strings.Contains(formatado, "segredo-que-nao-pode-aparecer") {
			t.Errorf("o segredo apareceu ao formatar a configuração: %s", formatado)
		}
	}
	if cfg.Credentials == nil || cfg.Credentials.SecretAccessKey != "segredo-que-nao-pode-aparecer" {
		t.Error("redigir na impressão não pode alterar o valor que o SDK recebe")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CA5 — prefixo sem barra final
// ─────────────────────────────────────────────────────────────────────────────

func TestCA5_PrefixoSemBarraFinalEhNormalizadoNuncaConcatenadoCru(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)
	t.Setenv("VAN_S3_PREFIX_OUTBOUND", "saida-homolog")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("prefixo sem barra deveria ser normalizado, não recusado: %v", err)
	}
	if cfg.Prefixes.Outbound != "saida-homolog/" {
		t.Errorf("prefixo = %q, esperava %q", cfg.Prefixes.Outbound, "saida-homolog/")
	}
	// A consequência que a normalização impede: sem a barra, `saida-homolog` + `X.REM` viraria
	// `saida-homologX.REM` — um objeto na raiz do bucket, que nenhuma listagem por prefixo encontra.
	if cfg.Prefixes.Outbound+"X.REM" != "saida-homolog/X.REM" {
		t.Errorf("a concatenação produziu %q", cfg.Prefixes.Outbound+"X.REM")
	}
}

func TestCA5_PrefixoComBarraInicialEhRecusadoComErroNomeado(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)
	t.Setenv("VAN_S3_PREFIX_STATUS", "/status/")

	_, err := config.Load()
	if err == nil {
		t.Fatal("barra inicial produz um segmento vazio na chave: `/status/X` é outro objeto que `status/X`")
	}
	var invalido config.InvalidPrefixError
	if !errors.As(err, &invalido) {
		t.Fatalf("esperava InvalidPrefixError; veio %T: %v", err, err)
	}
	if invalido.Field != "VAN_S3_PREFIX_STATUS" {
		t.Errorf("o erro deveria nomear a variável; veio %q", invalido.Field)
	}
}

func TestPrefixosPadraoBatemComOsDoContrato(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("configuração mínima: %v", err)
	}
	// Os defaults precisam ser os mesmos dos dois lados da fronteira: um agente que varresse um
	// prefixo e um emissor que depositasse noutro produziriam uma fila silenciosamente vazia.
	if cfg.Prefixes != bucket.DefaultPrefixes() {
		t.Errorf("prefixos = %+v, esperava %+v", cfg.Prefixes, bucket.DefaultPrefixes())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Endereçamento das chaves: por caminho ou por subdomínio
// ─────────────────────────────────────────────────────────────────────────────

func TestPathStyleSegueAHeuristicaDoCoreApiQuandoNaoConfigurado(t *testing.T) {
	casos := []struct {
		endpoint string
		esperado bool
	}{
		{"", false},                       // AWS: endereçamento por subdomínio
		{"http://localhost:9000", true},   // MinIO local: `bucket.localhost` não resolve
		{"http://127.0.0.1:9000", true},   //
		{"https://s3.exemplo.com", false}, // provedor com DNS de subdomínio
	}
	for _, c := range casos {
		t.Run(c.endpoint, func(t *testing.T) {
			limpaAmbiente(t)
			ambienteDoBucket(t)
			t.Setenv("VAN_S3_ENDPOINT", c.endpoint)

			cfg, err := config.LoadStorage()
			if err != nil {
				t.Fatalf("configuração: %v", err)
			}
			if cfg.ForcePathStyle != c.esperado {
				t.Errorf("ForcePathStyle = %t para endpoint %q, esperava %t",
					cfg.ForcePathStyle, c.endpoint, c.esperado)
			}
		})
	}
}

// O caso que motivou a variável: um S3-compatível que NÃO está em localhost passa pela heurística
// como se fosse a AWS, e o SDK vai procurar `bucket.192.0.2.10` — um subdomínio que não existe.
func TestPathStylePodeSerForcadoParaEndpointQueNaoEhLocal(t *testing.T) {
	limpaAmbiente(t)
	ambienteDoBucket(t)
	t.Setenv("VAN_S3_ENDPOINT", "http://192.0.2.10:9000")

	semOverride, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("configuração: %v", err)
	}
	if semOverride.ForcePathStyle {
		t.Error("sem override, o comportamento precisa continuar idêntico ao do core-api")
	}

	t.Setenv("VAN_S3_FORCE_PATH_STYLE", "true")
	comOverride, err := config.LoadStorage()
	if err != nil {
		t.Fatalf("configuração com override: %v", err)
	}
	if !comOverride.ForcePathStyle {
		t.Error("o override deveria ligar o endereçamento por caminho")
	}
}

// Valor que não dá para interpretar não pode cair no default em silêncio: quem escreveu a variável
// tinha uma intenção, e adivinhá-la produz um agente falando com o host errado achando que obedeceu.
func TestPathStyleComValorIlegivelFalhaNoBoot(t *testing.T) {
	limpaAmbiente(t)
	ambienteDoBucket(t)
	t.Setenv("VAN_S3_FORCE_PATH_STYLE", "talvez")

	if _, err := config.LoadStorage(); err == nil {
		t.Error("valor ilegível deveria falhar no boot")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Teto de comprimento do nome — configurável, e desligado por default
// ─────────────────────────────────────────────────────────────────────────────

func TestTetoDeNomeVemDoAmbienteENaoDeConstanteCompilada(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)
	t.Setenv("VAN_AGENT_NAME_MAX_LENGTH", "26")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("configuração: %v", err)
	}
	if cfg.NameMaxLength != 26 {
		t.Errorf("NameMaxLength = %d, esperava 26", cfg.NameMaxLength)
	}
}

// O default é SEM trava, e isso é deliberado: o teto efetivo depende da instalação e do parceiro
// (§11, p.26 — o procedimento do 1101 é condicional), e um default que recusasse por engano pararia
// a fila inteira sem que ninguém tivesse pedido.
func TestTetoDeNomeAusenteSignificaSemTrava(t *testing.T) {
	limpaAmbiente(t)
	ambienteDaMaquina(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("configuração: %v", err)
	}
	if cfg.NameMaxLength != 0 {
		t.Errorf("sem a variável, o teto precisa ficar em 0 (sem trava); veio %d", cfg.NameMaxLength)
	}
}

func TestTetoDeNomeInvalidoFalhaNoBoot(t *testing.T) {
	for _, valor := range []string{"vinte e seis", "-1"} {
		t.Run(valor, func(t *testing.T) {
			limpaAmbiente(t)
			ambienteDaMaquina(t)
			t.Setenv("VAN_AGENT_NAME_MAX_LENGTH", valor)

			if _, err := config.Load(); err == nil {
				t.Errorf("teto %q deveria falhar no boot, não virar comportamento silencioso", valor)
			}
		})
	}
}
