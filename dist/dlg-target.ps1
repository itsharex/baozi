Add-Type -AssemblyName System.Windows.Forms
$form = New-Object System.Windows.Forms.Form
$form.Text = "DialogTarget"
$form.Width = 260
$form.Height = 130
$form.StartPosition = "CenterScreen"
$form.Show()
$opening = $false
$timer = New-Object System.Windows.Forms.Timer
$timer.Interval = 1500
$timer.add_Tick({
  if ($script:opening) { return }
  $script:opening = $true
  $dlg = New-Object System.Windows.Forms.OpenFileDialog
  $dlg.Title = "Baozi dialog test"
  $dlg.CheckFileExists = $false
  [void]$dlg.ShowDialog($form)
  $script:opening = $false
})
$timer.Start()
[System.Windows.Forms.Application]::Run($form)
