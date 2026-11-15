locals {
  dimensions = { Service = "discord-management-bot" }
  metric_alarms = {
    heartbeat        = { metric = "AppHeartbeat", statistic = "Minimum", operator = "LessThanThreshold", threshold = 1, periods = 3, missing = "breaching" }
    delivery_backlog = { metric = "OldestPendingSeconds", statistic = "Maximum", operator = "GreaterThanThreshold", threshold = 300, periods = 3, missing = "notBreaching" }
    backup_age       = { metric = "BackupAgeSeconds", statistic = "Maximum", operator = "GreaterThanThreshold", threshold = 3900, periods = 2, missing = "notBreaching" }
  }
}
resource "aws_cloudwatch_metric_alarm" "app" {
  for_each            = local.metric_alarms
  alarm_name          = "${local.project}-${each.key}"
  alarm_description   = "Gateway infrastructure: ${each.key}"
  namespace           = local.metrics_namespace
  metric_name         = each.value.metric
  statistic           = each.value.statistic
  dimensions          = local.dimensions
  comparison_operator = each.value.operator
  threshold           = each.value.threshold
  evaluation_periods  = each.value.periods
  datapoints_to_alarm = each.value.periods
  period              = 60
  treat_missing_data  = each.value.missing
}
resource "aws_cloudwatch_metric_alarm" "gateway" {
  alarm_name          = "${local.project}-gateway-disconnected"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  evaluation_periods  = 5
  datapoints_to_alarm = 5
  treat_missing_data  = "notBreaching"
  metric_query {
    id = "expected"
    metric {
      namespace   = local.metrics_namespace
      metric_name = "GatewayExpected"
      dimensions  = local.dimensions
      period      = 60
      stat        = "Maximum"
    }
  }
  metric_query {
    id = "connected"
    metric {
      namespace   = local.metrics_namespace
      metric_name = "GatewayConnected"
      dimensions  = local.dimensions
      period      = 60
      stat        = "Maximum"
    }
  }
  metric_query {
    id          = "disconnected"
    expression  = "IF(expected > 0, 1 - connected, 0)"
    label       = "Expected but disconnected"
    return_data = true
  }
}
resource "aws_cloudwatch_metric_alarm" "instance" {
  alarm_name          = "${local.project}-instance-status"
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed"
  dimensions          = { InstanceId = aws_instance.gateway.id }
  statistic           = "Maximum"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 0
  evaluation_periods  = 2
  period              = 60
  treat_missing_data  = "notBreaching"
}
resource "aws_cloudwatch_metric_alarm" "memory" {
  alarm_name          = "${local.project}-host-memory"
  namespace           = "${local.metrics_namespace}Host"
  metric_name         = "mem_used_percent"
  dimensions          = { InstanceId = aws_instance.gateway.id }
  statistic           = "Maximum"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 90
  evaluation_periods  = 3
  period              = 60
  treat_missing_data  = "notBreaching"
}
resource "aws_cloudwatch_metric_alarm" "disk" {
  alarm_name          = "${local.project}-data-disk"
  namespace           = "${local.metrics_namespace}Host"
  metric_name         = "disk_used_percent"
  dimensions          = { InstanceId = aws_instance.gateway.id, path = "/var/lib/discord-gateway", fstype = "ext4" }
  statistic           = "Maximum"
  comparison_operator = "GreaterThanThreshold"
  threshold           = 85
  evaluation_periods  = 3
  period              = 60
  treat_missing_data  = "notBreaching"
}
locals {
  alarm_names = concat(
    [for alarm in aws_cloudwatch_metric_alarm.app : alarm.alarm_name],
    [aws_cloudwatch_metric_alarm.gateway.alarm_name, aws_cloudwatch_metric_alarm.instance.alarm_name, aws_cloudwatch_metric_alarm.memory.alarm_name, aws_cloudwatch_metric_alarm.disk.alarm_name]
  )
  notification_states = {
    alarm    = { state = { value = ["ALARM"] } }
    recovery = { state = { value = ["OK"] }, previousState = { value = ["ALARM"] } }
  }
}
resource "aws_cloudwatch_event_connection" "discord" {
  name               = "${local.project}-discord"
  authorization_type = "API_KEY"
  auth_parameters {
    api_key {
      key   = "Content-Type"
      value = "application/json"
    }
  }
}
resource "aws_cloudwatch_event_api_destination" "discord" {
  name                             = "${local.project}-discord"
  connection_arn                   = aws_cloudwatch_event_connection.discord.arn
  http_method                      = "POST"
  invocation_endpoint              = aws_ssm_parameter.discord_webhook_url.value
  invocation_rate_limit_per_second = 1
}
resource "aws_cloudwatch_event_rule" "notify" {
  for_each = local.notification_states
  name     = "${local.project}-${each.key}"
  state    = "ENABLED"
  event_pattern = jsonencode({
    source      = ["aws.cloudwatch"]
    detail-type = ["CloudWatch Alarm State Change"]
    account     = [local.account_id]
    detail      = merge({ alarmName = local.alarm_names }, each.value)
  })
}
resource "aws_iam_role" "notify" {
  name = "${local.project}-notify"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow", Action = "sts:AssumeRole", Principal = { Service = "events.amazonaws.com" }
      Condition = { StringEquals = { "aws:SourceAccount" = local.account_id } }
    }]
  })
}
resource "aws_iam_role_policy" "notify" {
  role = aws_iam_role.notify.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "events:InvokeApiDestination", Resource = aws_cloudwatch_event_api_destination.discord.arn }]
  })
}
resource "aws_cloudwatch_event_target" "notify" {
  for_each = aws_cloudwatch_event_rule.notify
  rule     = each.value.name
  arn      = aws_cloudwatch_event_api_destination.discord.arn
  role_arn = aws_iam_role.notify.arn
  retry_policy {
    maximum_event_age_in_seconds = 86400
    maximum_retry_attempts       = 185
  }
  http_target { query_string_parameters = { wait = "true" } }
  input_transformer {
    input_paths = {
      alarm = "$.detail.alarmName"
      state = "$.detail.state.value"
      time  = "$.time"
    }
    input_template = <<-JSON
      {"content":"Gateway 基盤通知 [<state>]\n<alarm>\n<time>"}
    JSON
  }
}
resource "aws_cloudwatch_dashboard" "gateway" {
  dashboard_name = local.project
  dashboard_body = jsonencode({
    start = "-PT6H"
    widgets = [
      {
        type = "metric", width = 12, height = 6, x = 0, y = 0
        properties = {
          title   = "Gateway接続", region = local.region, period = 60, stat = "Maximum"
          metrics = [[local.metrics_namespace, "GatewayConnected", "Service", "discord-management-bot"], [".", "GatewayExpected", ".", "."]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 12, y = 0
        properties = {
          title   = "未受付・結果待ち", region = local.region, period = 60, stat = "Maximum"
          metrics = [[local.metrics_namespace, "PendingDeliveries", "Service", "discord-management-bot"], [".", "AwaitingResult", ".", "."]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 0, y = 6
        properties = {
          title   = "Workerの結果", region = local.region, period = 300, stat = "Sum"
          metrics = [[local.metrics_namespace, "WorkerSuccess", "Service", "discord-management-bot"], [".", "WorkerPartialSuccess", ".", "."], [".", "WorkerFailure", ".", "."], [".", "WorkerSkipped", ".", "."]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 12, y = 6
        properties = {
          title   = "配信・処理時間 ms", region = local.region, period = 300, stat = "p95"
          metrics = [[local.metrics_namespace, "DeliveryHTTPMilliseconds", "Service", "discord-management-bot"], [".", "WorkerMilliseconds", ".", "."]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 0, y = 12
        properties = {
          title   = "EC2 CPUとクレジット", region = local.region, period = 300, stat = "Average"
          metrics = [["AWS/EC2", "CPUUtilization", "InstanceId", aws_instance.gateway.id], [".", "CPUCreditBalance", ".", "."]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 12, y = 12
        properties = {
          title   = "ホストメモリ使用率", region = local.region, period = 60, stat = "Maximum"
          metrics = [["${local.metrics_namespace}Host", "mem_used_percent", "InstanceId", aws_instance.gateway.id]]
        }
      },
      {
        type = "metric", width = 12, height = 6, x = 0, y = 18
        properties = {
          title   = "未受付の最長経過時間・バックアップ経過 秒", region = local.region, period = 60, stat = "Maximum"
          metrics = [[local.metrics_namespace, "OldestPendingSeconds", "Service", "discord-management-bot"], [".", "BackupAgeSeconds", ".", "."]]
        }
      },
      {
        type = "log", width = 12, height = 6, x = 12, y = 18
        properties = {
          title = "直近の処理ログ", region = local.region, view = "table"
          query = "SOURCE '${local.log_group}' | fields @timestamp, event, delivery_id, destination_id, status, error | sort @timestamp desc | limit 30"
        }
      }
    ]
  })
}
output "dashboard_url" {
  value = "https://${local.region}.console.aws.amazon.com/cloudwatch/home?region=${local.region}#dashboards/dashboard/${aws_cloudwatch_dashboard.gateway.dashboard_name}"
}
