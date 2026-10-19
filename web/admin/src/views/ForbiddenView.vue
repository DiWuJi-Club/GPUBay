<template>
  <el-result icon="error" title="无权访问该页面" sub-title="当前角色没有该功能的权限，如有需要请联系超级管理员" data-testid="forbidden-page">
    <template #extra><router-link to="/"><el-button type="primary" data-testid="forbidden-home">回到仪表盘</el-button></router-link></template>
  </el-result>
</template>
